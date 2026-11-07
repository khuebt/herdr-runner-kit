#!/usr/bin/env python3
"""Probe installed main runtimes with isolated profiles and a loopback provider.

Fixture modes use no SDK mocks, global extensions, credentials, or user state.
The transcript-contract case drives the real news child through RPC. The TUI
case uses the public kit launcher and production extension in a real PTY.
Both use runtime-owned headers and receipts that must precede cursor commit.
Any failed check exits nonzero and retains evidence under --out. The all case
runs the implemented suites, not the separate live-model release gate.
The live case uses an explicit provider config and credential source, never writes either,
and retains isolated transcripts for review of routing after compact/resume.
"""

import argparse
from datetime import datetime, timezone
import fcntl
import hashlib
import http.server
import json
import os
from pathlib import Path
import pty
import select
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import termios
import threading
import time


EXTENSION = r'''
import { appendFileSync, existsSync, readFileSync, unlinkSync } from "node:fs";
import { spawn } from "node:child_process";
const root = process.env.HRK_PROBE_OUT;
const log = (data) => appendFileSync(root + "/extension.jsonl", JSON.stringify({at:Date.now(),...data}) + "\n");
export default function(pi) {
  pi.registerProvider("hrk-probe", {
    baseUrl: process.env.HRK_PROBE_URL, apiKey: "local-fixture", api: "openai-completions",
    models: [{id:"probe",name:"Local transcript probe",reasoning:false,input:["text"],
      cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:32000,maxTokens:2048}]
  });
  let timer, child, pending, generation;
  let bridgeID = 0;
  const send = (type, requestId, more={}) => {
    child.stdin.write(JSON.stringify({version:1,type,requestId,generation,...more}) + "\n");
  };
  const stopBridge = async (signal="SIGTERM") => {
    const previous = child;
    child = undefined;
    if (!previous || previous.exitCode !== null || previous.signalCode !== null) return;
    await new Promise(resolve => {
      const timeout = setTimeout(() => previous.kill("SIGKILL"), 1000);
      previous.once("exit", () => { clearTimeout(timeout); resolve(); });
      previous.kill(signal);
    });
  };
  const startBridge = (ctx) => {
    const current = spawn(process.env.HRK_PROBE_KIT,["main","__news"], {
      env:{...process.env,HRK_MAIN:String(process.pid),HRK_MAIN_HARNESS:process.env.HRK_PROBE_HARNESS}, stdio:["pipe","pipe","pipe"]
    });
    child = current;
    const id = ++bridgeID;
    let buffer = "";
    log({event:"bridge_start",bridge:id,pid:current.pid,parentPID:process.pid});
    current.stdin.on("error", error => log({event:"bridge_stdin_error",bridge:id,message:error.message}));
    current.on("error", error => log({event:"bridge_error",bridge:id,message:error.message}));
    current.on("exit", (code,signal) => log({event:"bridge_exit",bridge:id,code,signal}));
    current.stderr.on("data", data => appendFileSync(root+"/bridge-stderr.txt",data));
    current.stdout.on("data", data => {
      buffer += data.toString();
      if (buffer.length > 4*1024*1024) throw new Error("oversized bridge response");
      let newline;
      while ((newline=buffer.indexOf("\n")) >= 0) {
        const response=JSON.parse(buffer.slice(0,newline)); buffer=buffer.slice(newline+1);
        log({event:"bridge_message",bridge:id,response});
        if (child !== current) continue;
        generation=response.generation;
        if(response.type === "batch") pending=response.batch;
        if(response.type === "committed" && pending?.batchId === response.batchId) pending=undefined;
      }
    });
    current.stdin.write(JSON.stringify({version:1,type:"register",requestId:"register-"+id,
      harness:process.env.HRK_PROBE_HARNESS,sessionId:ctx.sessionManager.getSessionId(),
      sessionFile:ctx.sessionManager.getSessionFile(),cwd:ctx.sessionManager.getCwd(),
      parentPID:process.pid,nonce:process.env.HRK_MAIN_NONCE,quiesced:true})+"\n");
  };
  pi.on("session_start", (_event, ctx) => {
    log({event:"session_start",id:ctx.sessionManager.getSessionId(),file:ctx.sessionManager.getSessionFile(),cwd:ctx.sessionManager.getCwd(),pid:process.pid});
    startBridge(ctx);
    timer=setInterval(async () => {
      const file=root+"/inbox.json";
      if(!existsSync(file)) return;
      const action=JSON.parse(readFileSync(file,"utf8")); unlinkSync(file);
      try {
        switch(action.kind) {
          case "append":
            if(!pending) throw new Error("no pending batch");
            pi.sendMessage({customType:"hrk-news",content:pending.lines.join("\n"),display:true,
              details:{batchId:pending.batchId,owner:process.env.HRK_MAIN_OWNER,digest:pending.digest}},
              {deliverAs:"followUp",triggerTurn:true});
            log({event:"append_requested",batchId:pending.batchId,idle:ctx.isIdle()});
            break;
          case "receipt": send("receipt",action.id,{batchId:action.batchId}); break;
          case "restart": await stopBridge("SIGKILL"); startBridge(ctx); break;
          case "abort": await ctx.abort(); break;
          case "close": send("close",action.id,{quiesced:true}); break;
          case "activity": send("activity",action.id); break;
          case "snapshot": break;
          default: throw new Error("unknown probe action");
        }
        log({event:"action_done",id:action.id,idle:ctx.isIdle()});
      } catch(error) {log({event:"action_error",id:action.id,message:String(error)});}
    },20);
  });
  for(const name of ["agent_start","agent_end","agent_settled","session_before_compact","session_compact"]) {
    pi.on(name,(_event,ctx)=>log({event:name,idle:ctx.isIdle()}));
  }
  pi.on("session_shutdown",async () => {clearInterval(timer); await stopBridge();log({event:"session_shutdown"});});
  pi.on("message_end",event=>log({event:"message_end",message:event.message}));
}
'''


# Observation only: the production extension owns all delivery and input gates.
TUI_EXTENSION = r'''
import { appendFileSync, existsSync, readFileSync } from "node:fs";
const root = process.env.HRK_PROBE_OUT;
const log = data => appendFileSync(root + "/observer.jsonl", JSON.stringify({at:Date.now(),...data}) + "\n");
const identity = ctx => ({id:ctx.sessionManager.getSessionId(),file:ctx.sessionManager.getSessionFile()});
export default function(api) {
  const live=process.env.HRK_PROBE_LIVE_PROVIDER;
  const provider=live ? JSON.parse(live) : null;
  api.registerProvider(provider?.name || "hrk-probe", provider ? {
    ...provider.config,apiKey:process.env.HRK_PROBE_LIVE_API_KEY,
  } : {
    baseUrl:process.env.HRK_PROBE_URL, apiKey:"local-fixture", api:"openai-completions",
    models:[{id:"probe",name:"Local acceptance fixture",reasoning:false,input:["text"],
      cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:32000,maxTokens:2048}]
  });
  api.registerTool({
    name:"hrk_probe_hold", label:"Acceptance hold", description:"Hold a real tool execution until released by the acceptance driver.",
    parameters:{type:"object",properties:{},additionalProperties:false},
    async execute(_id,_params,signal) {
      log({event:"probe_tool_start"});
      try {
        while (!existsSync(root + "/release-tool")) {
          if (signal?.aborted) throw new Error("Acceptance tool cancelled");
          await new Promise(resolve => setTimeout(resolve, 20));
        }
        return {content:[{type:"text",text:"Acceptance tool completed."}],details:{}};
      } finally { log({event:"probe_tool_end"}); }
    }
  });
  api.on("input", event => {
    if (event.text === "Handled acceptance input") {
      log({event:"input_handled"});
      return {action:"handled",handled:true};
    }
  });
  api.registerCommand("probe-branch", {
    description:"Exercise the runtime's real branch operation.",
    handler:async (_args,ctx) => {
      const entry=ctx.sessionManager.getEntries().filter(e => e.type === "message" && e.message.role === "user").at(-1);
      if (!entry) throw new Error("Branch acceptance requires a user entry");
      let next=identity(ctx);
      let result;
      if (typeof ctx.branch === "function") {
        result=await ctx.branch(entry.id); next=identity(ctx);
      } else {
        result=await ctx.fork(entry.id,{withSession:c => {next=identity(c);}});
      }
      log({event:"branch_result",...result,...next});
    }
  });
  api.registerCommand("probe-resume", {
    description:"Exercise the runtime's session switch operation.",
    handler:async (_args,ctx) => {
      const path=readFileSync(root + "/resume-target.txt","utf8").trim();
      let next=identity(ctx);
      const omp=typeof ctx.branch === "function";
      const result=await ctx.switchSession(path,{withSession:c => {next=identity(c);}});
      if (omp) next=identity(ctx);
      log({event:"switch_result",...result,...next});
    }
  });
  let vetoSwitch=false;
  for (const event of ["session_before_switch","session_before_tree",process.env.HRK_MAIN_HARNESS === "omp" ? "session_before_branch" : "session_before_fork"])
    api.on(event, () => vetoSwitch ? {cancel:true} : undefined);
  api.registerCommand("probe-veto", {
    description:"Cancel a new session in a hook registered after the main guard.",
    handler:async (operation,ctx) => {
      vetoSwitch=true;
      try {
        let result;
        if (operation === "branch" || operation === "tree") {
          const entry=ctx.sessionManager.getEntries().filter(e => e.type === "message" && e.message.role === "user").at(-1);
          result=operation === "tree" ? await ctx.navigateTree(entry.id,{summarize:false}) :
            await (typeof ctx.branch === "function" ? ctx.branch(entry.id) : ctx.fork(entry.id));
        } else result=await ctx.newSession();
        log({event:"later_transition_veto",operation,...result});
      }
      finally { vetoSwitch=false; }
    }
  });
  api.registerCommand("probe-compact", {
    description:"Start compaction through the extension API.",
    handler:async (_args,ctx) => {
      ctx.compact({customInstructions:"Preserve acceptance history.",
        onComplete:() => log({event:"extension_compact_complete"}),
        onError:error => log({event:"extension_compact_error",message:String(error)})});
    }
  });
  let timer, tui;
  api.on("session_start", (_event,ctx) => {
    api.setActiveTools(live ? ["read","bash"] : ["hrk_probe_hold"]);
    clearInterval(timer);
    ctx.ui.setWidget("hrk-probe-observer", host => {
      tui = host;
      return {render:() => [],invalidate() {}};
    });
    log({event:"start",pid:process.pid,id:ctx.sessionManager.getSessionId(),file:ctx.sessionManager.getSessionFile(),
      fileExists:existsSync(ctx.sessionManager.getSessionFile() || "")});
    timer=setInterval(() => log({event:"snapshot",editor:ctx.ui.getEditorText(),idle:ctx.isIdle(),
      queued:ctx.hasPendingMessages(),id:ctx.sessionManager.getSessionId(),file:ctx.sessionManager.getSessionFile(),
      fileExists:existsSync(ctx.sessionManager.getSessionFile() || ""),
      focus:(tui?.getFocusedComponent?.() ?? tui?.getFocused?.())?.constructor?.name,
      overlay:tui?.hasOverlay?.()}),100);
  });
  for (const name of ["agent_start","agent_end","agent_settled","session_before_compact","session_compact","session_compact_failed","auto_compaction_start","auto_compaction_end","auto_retry_start","auto_retry_end","session_before_tree","session_tree"]) {
    api.on(name, event => {
      log({event:name,aborted:event.aborted,reason:event.reason,willContinue:event.willContinue,errorMessage:event.errorMessage});
      if (name === "session_before_compact") event.signal?.addEventListener("abort",
        () => log({event:"compaction_aborted"}), {once:true});
    });
  }
  api.on("session_shutdown", () => { clearInterval(timer); log({event:"shutdown"}); });
}
'''


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def jsonl(path, *, live_log=False):
    if not path.exists():
        return []
    raw = path.read_bytes()
    if raw and not raw.endswith(b"\n"):
        if not live_log:
            raise ValueError(f"incomplete JSONL: {path.name}")
        raw = raw[:raw.rfind(b"\n") + 1]
    return [json.loads(line) for line in raw.splitlines()]


def until(predicate, process, label, timeout=25):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        if predicate():
            return
        if process.poll() is not None:
            raise RuntimeError(f"runtime exited ({process.returncode}) while {label}")
        time.sleep(0.02)
    raise TimeoutError(label)


def stop(process, reader):
    forced = False
    if process.poll() is None:
        try:
            process.stdin.close()
        except BrokenPipeError:
            pass
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            process.terminate()
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                forced = True
                process.kill()
                process.wait(timeout=3)
    reader.join()
    process.stdout.close()
    return forced


def process_alive(pid):
    result = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, text=True, check=False)
    return bool(result.stdout.strip()) and not result.stdout.strip().startswith("Z")


def interrupt(_signum, _frame):
    raise InterruptedError("received SIGTERM")


def run(args):
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    binary = shutil.which(args.harness)
    if binary is None or not args.kit_bin.is_file():
        raise RuntimeError("installed harness and built --kit-bin are required")
    # Start from an allowlist so provider credentials cannot reach the runtime.
    env = {key: os.environ[key] for key in ("HOME", "PATH", "LANG", "LC_ALL", "TZ", "TERM") if key in os.environ}
    env.update({
        "HRK_PROBE_OUT": str(out), "HRK_MAIN_OWNER": str(out),
        "HRK_PROBE_KIT": str(args.kit_bin.resolve()), "HRK_PROBE_HARNESS": args.harness,
        "HRK_STATE_DIR": str(out / "kit-state"), "HRK_TELEGRAM": "off",
        "PI_CODING_AGENT_DIR": str(out / "agent"),
        "PI_CODING_AGENT_SESSION_DIR": str(out / "sessions"),
        "PI_CONFIG_DIR": os.path.relpath(out / "omp-root", Path.home()),
        "PI_OFFLINE": "1", "XDG_CONFIG_HOME": str(out / "config"),
        "XDG_STATE_HOME": str(out / "state"), "XDG_CACHE_HOME": str(out / "cache"),
    })
    (out / "agent").mkdir()
    write_json(out / "agent/settings.json", {"compaction": {"enabled": False, "reserveTokens": 512, "keepRecentTokens": 100}})
    (out / "probe.ts").write_text(EXTENSION)
    (out / "system.txt").write_text("Reply only to this local transport probe.\n")
    result = {"harness": args.harness, "checks": {}, "blockers": [], "kitBin": str(args.kit_bin.resolve())}
    version = subprocess.run([binary, "--version"], env=env, cwd=out, capture_output=True, text=True, timeout=15, check=True)
    result["version"] = version.stdout.strip()
    provider_started = threading.Event()
    provider_release = threading.Event()

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            length = int(self.headers.get("Content-Length", "0"))
            if length > 4 * 1024 * 1024:
                self.send_error(413)
                return
            payload = json.loads(self.rfile.read(length))
            with (out / "provider.jsonl").open("a") as log:
                log.write(json.dumps({"messages": payload.get("messages", [])}) + "\n")
            provider_started.set()
            if not provider_release.wait(30):
                self.send_error(504)
                return
            try:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for delta, finish in [({"role": "assistant", "content": "Local fixture response."}, None), ({}, "stop")]:
                    chunk = {"id": "probe", "object": "chat.completion.chunk", "created": int(time.time()), "model": "probe", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
                    self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                    self.wfile.flush()
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass

    # Deterministic per output directory. A busy port fails, never steals an owner.
    port = 20000 + int(hashlib.sha256(str(out).encode()).hexdigest()[:4], 16) % 20000
    result["provider"] = {"pid": os.getpid(), "port": port, "command": [sys.executable, *sys.argv], "worktree": str(Path.cwd())}
    env["HRK_PROBE_URL"] = f"http://127.0.0.1:{port}/v1"
    argv = [binary, "--mode", "rpc", "--no-extensions", "--no-skills", "--no-tools", "--session-dir", str(out / "sessions"), "--append-system-prompt", str(out / "system.txt"), "--model", "hrk-probe/probe", "--thinking", "off"]
    if args.harness == "pi":
        argv += ["--no-mcp", "--no-context-files", "--no-prompt-templates", "--no-themes", "--offline", "-e", str(out / "probe.ts")]
    else:
        (out / "omp.yml").write_text("setupVersion: 2\ncompaction:\n  enabled: false\n  reserveTokens: 512\n  keepRecentTokens: 100\ntui:\n  titleState: false\n")
        argv += ["--no-rules", "--no-lsp", "--no-title", "--config", str(out / "omp.yml"), "--hook", str(out / "probe.ts")]
    process = None
    reader = None
    server = None
    thread = None
    records = []
    stderr = (out / "stderr.txt").open("w")

    def start(command):
        child_env = dict(env, HRK_MAIN_NONCE=os.urandom(16).hex())
        child = subprocess.Popen(command, cwd=out, env=child_env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, text=True, bufsize=1)
        try:
            with (out / "processes.jsonl").open("a") as log:
                log.write(json.dumps({"pid": child.pid, "argv": command, "cwd": str(out), "port": port}) + "\n")
        except BaseException:
            child.kill()
            child.wait(timeout=3)
            child.stdin.close()
            child.stdout.close()
            raise

        def read():
            with (out / "rpc.jsonl").open("a") as log:
                for line in child.stdout:
                    log.write(line)
                    log.flush()
                    try:
                        records.append(json.loads(line))
                    except ValueError:
                        pass

        worker = threading.Thread(target=read)
        worker.start()
        return child, worker

    def events():
        return jsonl(out / "extension.jsonl", live_log=True)

    def send(data):
        process.stdin.write(json.dumps(data) + "\n")
        process.stdin.flush()

    def dispatch(action):
        temporary = out / "inbox.tmp"
        write_json(temporary, action)
        temporary.replace(out / "inbox.json")

    action_sequence = 0

    def action(kind, **values):
        nonlocal action_sequence
        action_sequence += 1
        identity = "action-" + str(action_sequence)
        dispatch({"kind": kind, "id": identity, **values})
        until(lambda: any(e.get("id") == identity for e in events()), process, kind)
        response = next(e for e in events() if e.get("id") == identity)
        if response["event"] == "action_error":
            raise RuntimeError(response["message"])
        return response

    def messages():
        return [e["response"] for e in events() if e["event"] == "bridge_message"]

    def wait_message(kind, *, batch=None, after=0, request=None):
        def matching():
            return [m for m in messages()[after:] if m["type"] == kind and
                    (batch is None or m.get("batchId") == batch) and
                    (request is None or m.get("requestId") == request)]
        until(matching, process, "bridge " + kind)
        return matching()[0]

    def idle():
        end = time.monotonic() + 20
        while time.monotonic() < end:
            if action("snapshot")["idle"]:
                return
            time.sleep(0.05)
        raise TimeoutError("runtime idle")

    def entries(path, batch):
        return [entry for entry in jsonl(path) if entry.get("type") == "custom_message" and
                entry.get("customType") == "hrk-news" and entry.get("details", {}).get("batchId") == batch]

    def news_state():
        return json.loads(next((out / "kit-state").glob("main-news-*.json")).read_text())

    def cursor():
        name = hashlib.sha256(str(out).encode()).hexdigest()[:12]
        return int((out / "kit-state" / ("cursor-" + name)).read_text())

    def emit_news():
        # Synthetic runner news enters the real on-disk event log under its flock.
        offset = len(messages())
        event = {"v": 1, "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                 "owner": str(out), "task": "t1", "type": "needs-human", "question": "Local transcript probe"}
        with (out / "kit-state/events.jsonl").open("ab") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX)
            handle.write((json.dumps(event) + "\n").encode())
            handle.flush()
            os.fsync(handle.fileno())
        return wait_message("batch", after=offset)["batch"]

    def receipt(batch):
        response = action("receipt", batchId=batch["batchId"])
        wait_message("committed", batch=batch["batchId"], request=response["id"])
        assert news_state().get("pending") is None, "pending survived verified commit"
        assert cursor() == batch["to"], "cursor not at committed batch end"

    try:
        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        transcript = out / "bootstrap.jsonl"
        transcript.touch(mode=0o600, exist_ok=False)
        resume_flag = "--session" if args.harness == "pi" else "--resume"
        command = argv + [resume_flag, str(transcript)]
        process, reader = start(command)
        until(lambda: any(e["event"] == "session_start" for e in events()), process, "session start")
        identity = next(e for e in events() if e["event"] == "session_start")
        result["identity"] = identity
        wait_message("ready")
        header = next(e for e in jsonl(transcript) if e.get("type") == "session")
        assert (header["id"], header["cwd"], identity["file"]) == (identity["id"], str(out), str(transcript))
        result["checks"]["bootstrap_header_before_registration"] = True
        activity = action("activity")
        wait_message("ready", request=activity["id"])
        result["checks"]["activity_while_waiting"] = True

        batch = emit_news()
        assert cursor() == batch["from"], "cursor advanced before append"
        attempt = action("receipt", batchId=batch["batchId"])
        wait_message("error", request=attempt["id"])
        assert news_state()["pending"]["batchId"] == batch["batchId"]
        action("append")
        until(provider_started.is_set, process, "first provider request")
        until(lambda: len(entries(transcript, batch["batchId"])) == 1, process, "disk custom entry")
        receipt(batch)
        assert not provider_release.is_set(), "provider completed before receipt check"
        result["checks"]["disk_receipt_before_model_reply"] = True
        receipt(batch)
        result["checks"]["duplicate_receipt"] = True
        provider_release.set()
        idle()

        # Persisted append without ACK: same-parent reconnect ACKs, never re-emits.
        batch = emit_news()
        action("append")
        until(lambda: len(entries(transcript, batch["batchId"])) == 1, process, "append before lost receipt")
        offset = len(messages())
        action("restart")
        wait_message("committed", batch=batch["batchId"], after=offset)
        assert not any(m["type"] == "batch" for m in messages()[offset:]), "duplicate append after receipt recovery"
        result["checks"]["append_before_ack_recovery"] = True
        idle()
        shutil.copyfile(transcript, out / "persisted.jsonl")

        # A real busy followUp remains inside the old runtime while its child dies.
        provider_started.clear()
        provider_release.clear()
        send({"id": "busy", "type": "prompt", "message": "Hold this local provider response."})
        until(provider_started.is_set, process, "busy provider request")
        batch = emit_news()
        action("append")
        assert not entries(transcript, batch["batchId"]), "busy followUp was not queued"
        offset = len(messages())
        action("restart")
        wait_message("conflict", batch=batch["batchId"], after=offset)
        assert not any(m["type"] == "batch" for m in messages()[offset:]), "live-parent replay"
        assert cursor() == batch["from"], "cursor advanced on uncertain queued append"
        provider_release.set()
        until(lambda: len(entries(transcript, batch["batchId"])) == 1, process, "late runtime append")
        receipt(batch)
        idle()
        result["checks"]["living_parent_late_append_fenced"] = True

        send({"id": "padding", "type": "prompt", "message": "padding " * 1500})
        until(lambda: any(r.get("id") == "padding" for r in records), process, "padding prompt")
        idle()
        send({"id": "compact", "type": "compact", "customInstructions": "Preserve news."})
        until(lambda: any(r.get("id") == "compact" for r in records), process, "compact")
        assert any(r.get("id") == "compact" and r.get("success") for r in records), "compaction failed"
        assert len(entries(transcript, batch["batchId"])) == 1
        result["checks"]["compact_retains_physical_receipt"] = True

        # Cancel with an uncertain append, then kill the owning runtime. Only a
        # new parent may replay, and only if the original transcript has no receipt.
        provider_started.clear()
        provider_release.clear()
        send({"id": "cancel-busy", "type": "prompt", "message": "Cancel this held response."})
        until(provider_started.is_set, process, "cancel provider request")
        batch = emit_news()
        action("append")
        action("abort")
        old_child = [e for e in events() if e["event"] == "bridge_start"][-1]["pid"]
        process.kill()
        process.wait(timeout=3)
        stop(process, reader)
        provider_release.set()
        deadline = time.monotonic() + 7
        while process_alive(old_child):
            if time.monotonic() > deadline:
                raise TimeoutError("orphan child cleanup")
            time.sleep(0.02)
        result["checks"]["parent_sigkill_child_cleanup"] = True
        offset = len(messages())
        starts = sum(e["event"] == "session_start" for e in events())
        process, reader = start(command)
        until(lambda: sum(e["event"] == "session_start" for e in events()) > starts, process, "resume")
        resumed = [e for e in events() if e["event"] == "session_start"][-1]
        assert (resumed["id"], resumed["file"]) == (identity["id"], identity["file"]), "resume changed target"
        wait_message("ready", after=offset)
        until(lambda: any(m["type"] in ("batch", "committed") for m in messages()[offset:]), process, "recovery resolution")
        resolution = next(m for m in messages()[offset:] if m["type"] in ("batch", "committed"))
        assert resolution["batchId"] == batch["batchId"], "recovery changed batch"
        if resolution["type"] == "batch":
            assert not entries(transcript, batch["batchId"]), "replayed persisted entry"
            action("append")
            until(lambda: len(entries(transcript, batch["batchId"])) == 1, process, "replayed disk entry")
            receipt(batch)
        assert len(entries(transcript, batch["batchId"])) == 1, "recovery duplicated custom entry"
        assert cursor() == batch["to"]
        result["checks"]["cancel_and_dead_parent_same_target_recovery"] = True
        result["recoveryResolution"] = resolution["type"]
        idle()
        close = action("close")
        wait_message("ready", request=close["id"])
        assert news_state().get("lease") is None, "quiesced close kept lease"
        result["checks"]["quiesced_close_releases_lease"] = True
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
        if isinstance(error, InterruptedError):
            raise
    finally:
        provider_release.set()
        if process is not None and reader is not None:
            if stop(process, reader):
                result["blockers"].append("RPC main required SIGKILL during cleanup")
        for event in events():
            if event["event"] != "bridge_start" or not process_alive(event["pid"]):
                continue
            try:
                os.kill(event["pid"], 15)
                deadline = time.monotonic() + 2
                while process_alive(event["pid"]) and time.monotonic() < deadline:
                    time.sleep(0.02)
                if process_alive(event["pid"]):
                    result["blockers"].append("RPC news child required SIGKILL during cleanup")
                    os.kill(event["pid"], 9)
                    deadline = time.monotonic() + 2
                    while process_alive(event["pid"]) and time.monotonic() < deadline:
                        time.sleep(0.02)
                if process_alive(event["pid"]):
                    result["blockers"].append("RPC news child survived cleanup")
            except ProcessLookupError:
                pass
        if server is not None:
            server.shutdown()
            server.server_close()
        if thread is not None:
            thread.join()
        stderr.close()
        write_json(out / "result.json", result)
    print(json.dumps({"harness": args.harness, "version": result.get("version"), "checks": result["checks"], "blockers": result["blockers"], "error": result.get("error"), "out": str(out)}))
    return 1 if result.get("error") or result["blockers"] or any(value is not True for value in result["checks"].values()) else 0


def run_tui(args):
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    binary = shutil.which(args.harness)
    if binary is None or not args.kit_bin.is_file():
        raise RuntimeError("installed harness and built --kit-bin are required")
    env = {key: os.environ[key] for key in ("HOME", "PATH", "LANG", "LC_ALL", "TZ") if key in os.environ}
    env.update({
        "HRK_PROBE_OUT": str(out), "HRK_STATE_DIR": str(out / "kit-state"),
        "HRK_TELEGRAM": "off", "TERM": "xterm-256color",
        "PI_CODING_AGENT_DIR": str(out / "agent"),
        "PI_CODING_AGENT_SESSION_DIR": str(out / "sessions"),
        "PI_CONFIG_DIR": os.path.relpath(out / "omp-root", Path.home()), "PI_OFFLINE": "1",
        "XDG_CONFIG_HOME": str(out / "config"), "XDG_STATE_HOME": str(out / "state"),
        "XDG_CACHE_HOME": str(out / "cache"),
    })
    live = args.case == "live"
    if live:
        provider_name, model_id = args.live_model.split("/", 1)
        configured = json.loads(args.live_provider_config.read_text())["providers"][provider_name]
        model = next(m for m in configured["models"] if m["id"] == model_id)
        env["HRK_PROBE_LIVE_PROVIDER"] = json.dumps({"name": provider_name, "config": {
            "baseUrl": configured["baseUrl"], "api": configured["api"], "models": [model]}})
        credential_spec = configured["apiKey"]
        if credential_spec.startswith("!"):
            try:
                credential = subprocess.run(credential_spec[1:], shell=True, executable="/bin/sh",
                                            capture_output=True, text=True, timeout=15)
            except (OSError, subprocess.TimeoutExpired):
                raise RuntimeError("live credential helper failed") from None
            if credential.returncode:
                raise RuntimeError("live credential helper failed")
            value = credential.stdout.strip()
        else:
            value = os.environ.get(credential_spec, credential_spec).strip()
        if not value or "\n" in value or "\r" in value or len(value) > 16384:
            raise RuntimeError("live credential source must return one nonempty header-safe value")
        env["HRK_PROBE_LIVE_API_KEY"] = value
    (out / "agent").mkdir()
    (out / "bin").mkdir()
    write_json(out / "agent/settings.json", {"compaction": {"enabled": True, "reserveTokens": 512, "keepRecentTokens": 100}})
    (out / "observer.ts").write_text(TUI_EXTENSION)
    flags = ["--no-extensions", "--no-skills", "--session-dir", str(out / "sessions")]
    if args.harness == "pi":
        flags += ["--no-builtin-tools", "--no-mcp", "--no-context-files", "--no-prompt-templates", "--no-themes", "--offline", "-e", str(out / "observer.ts")]
    else:
        (out / "omp.yml").write_text("setupVersion: 2\ncompaction:\n  enabled: true\n  reserveTokens: 512\n  keepRecentTokens: 100\ntui:\n  titleState: false\n")
        flags += ["--no-tools", "--no-rules", "--no-lsp", "--no-title", "--config", str(out / "omp.yml"), "--hook", str(out / "observer.ts")]
    if live:
        flags.remove("--no-builtin-tools" if args.harness == "pi" else "--no-tools")
        flags += ["--tools", "read,bash"]
    # The public launcher has no arbitrary runtime flags. This executable only
    # adds profile isolation and the observer, then execs the installed runtime.
    launcher = out / "bin" / args.harness
    # Load the fixture after HRK so its handled input exercises HRK's closed gate.
    launcher.write_text("#!/bin/sh\nexec " + shlex.quote(binary) + ' "$@" ' + shlex.join(flags) + "\n")
    launcher.chmod(0o700)
    env["PATH"] = str(out / "bin") + os.pathsep + env["PATH"]
    port = 20000 + int(hashlib.sha256(str(out).encode()).hexdigest()[:4], 16) % 20000
    env["HRK_PROBE_URL"] = f"http://127.0.0.1:{port}/v1"
    result = {"harness": args.harness, "case": "tui", "kitBin": str(args.kit_bin.resolve()), "checks": {}, "blockers": []}
    result["provider"] = {"pid": os.getpid(), "port": port, "command": [sys.executable, *sys.argv], "worktree": str(Path.cwd())}
    result["version"] = subprocess.check_output([binary, "--version"], cwd=out, env=env, text=True, timeout=15).strip()
    if live:
        result["case"] = "live"
        result["provider"] = {"model": args.live_model, "api": configured["api"],
                              "configSource": str(args.live_provider_config),
                              "credentialSource": "configured command" if credential_spec.startswith("!") else "configured key"}
    provider_started = threading.Event()
    provider_release = threading.Event()
    provider_release.set()
    provider_status = 200
    provider_usage = 0
    compaction_status = 200
    provider_tool = False
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_POST(self):
            nonlocal provider_usage, provider_status, provider_tool
            length = int(self.headers.get("Content-Length", "0"))
            if length > 4 * 1024 * 1024:
                self.send_error(413)
                return
            payload = json.loads(self.rfile.read(length))
            with (out / "provider.jsonl").open("a") as log:
                log.write(json.dumps(payload) + "\n")
            status = provider_status
            usage = provider_usage
            provider_usage = 0
            tool = provider_tool
            provider_tool = False
            provider_started.set()
            if not provider_release.wait(30):
                self.send_error(504, "acceptance probe did not release provider")
                return
            try:
                if status != 200:
                    self.send_response(status)
                    self.send_header("Content-Type", "text/plain")
                    self.end_headers()
                    self.wfile.write(b"Local acceptance provider failure")
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                if usage:
                    # Hold the next provider call, made by real auto-compaction.
                    provider_status = compaction_status
                    provider_release.clear()
                    provider_started.clear()
                deltas = [({"role": "assistant", "content": "Local fixture complete."}, None), ({}, "stop")]
                if tool:
                    deltas = [({"role": "assistant", "tool_calls": [{"index": 0, "id": "probe-hold", "type": "function",
                               "function": {"name": "hrk_probe_hold", "arguments": "{}"}}]}, None), ({}, "tool_calls")]
                for delta, finish in deltas:
                    chunk = {"id": "probe", "object": "chat.completion.chunk", "created": int(time.time()), "model": "probe", "choices": [{"index": 0, "delta": delta, "finish_reason": finish}]}
                    self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                if usage:
                    chunk = {"id": "probe", "object": "chat.completion.chunk", "created": int(time.time()), "model": "probe",
                             "choices": [], "usage": {"prompt_tokens": usage, "completion_tokens": 10, "total_tokens": usage + 10}}
                    self.wfile.write(("data: " + json.dumps(chunk) + "\n\n").encode())
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass

    process = master = server = worker = None
    screen = bytearray()
    processes = []
    bridges = set()
    transcript = None

    def events():
        return jsonl(out / "observer.jsonl", live_log=True)

    def state():
        path = out / "kit-state" / ("main-news-" + hashlib.sha256(str(out).encode()).hexdigest()[:12] + ".json")
        return json.loads(path.read_text()) if path.exists() else {}

    def children(parent=None):
        parent = process.pid if parent is None else parent
        rows = subprocess.check_output(["ps", "-eo", "pid=,pgid=,args="], text=True)
        current = []
        for row in rows.splitlines():
            fields = row.split(None, 2)
            if len(fields) == 3 and int(fields[1]) == parent and fields[2] == str(args.kit_bin.resolve()) + " main __news":
                child = int(fields[0])
                current.append(child)
                if child not in bridges:
                    processes.append({"pid": child, "argv": [str(args.kit_bin.resolve()), "main", "__news"],
                                      "cwd": str(out), "worktree": str(Path.cwd()), "parentPID": parent})
                    bridges.add(child)
                    write_json(out / "processes.json", processes)
        return current

    def entries(batch):
        paths = {Path(e["file"]) for e in events() if e.get("file")}
        found = {}
        for path in paths:
            file_ids = set()
            for entry in jsonl(path, live_log=True):
                if entry.get("customType") != "hrk-news" or entry.get("details", {}).get("batchId") != batch:
                    continue
                # Runtime forks copy historical entries with their original IDs.
                key = entry.get("id") or (str(path), len(found))
                assert key not in file_ids, "one transcript repeated a receipt entry"
                file_ids.add(key)
                assert key not in found or found[key] == entry, "fork changed a copied receipt"
                found[key] = entry
        return list(found.values())

    def pump(seconds):
        end = time.monotonic() + seconds
        while time.monotonic() < end:
            ready, _, _ = select.select([master], [], [], min(0.05, max(0, end - time.monotonic())))
            if ready:
                try:
                    screen.extend(os.read(master, 65536))
                except OSError:
                    break

    def wait_for(predicate, label, timeout=20):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if predicate():
                return
            if process.poll() is not None:
                raise RuntimeError(f"runtime exited ({process.returncode}): {label}")
            pump(0.05)
        raise TimeoutError(label)

    def send(data):
        with (out / "input.jsonl").open("a") as log:
            log.write(json.dumps({"at": time.time_ns() // 1000000, "bytes": data.decode("utf8")}) + "\n")
        os.write(master, data)

    def command(value):
        send(value.encode())
        pump(0.2)
        send(b"\r")

    def snapshot():
        return next((e for e in reversed(events()) if e["event"] == "snapshot"), {})

    def editor(value):
        wait_for(lambda: snapshot().get("editor") == value, "editor bytes " + repr(value))

    def start(resume=None):
        nonlocal process, master, transcript
        model_spec = args.live_model + ":low" if live else "hrk-probe/probe:off"
        argv = [str(args.kit_bin.resolve()), "main", "--agent", args.harness + ":" + model_spec, "--exec"]
        if resume:
            argv += ["--resume", str(resume)]
        master, slave = pty.openpty()
        try:
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
            process = subprocess.Popen(argv, cwd=out, env=env, stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
        finally:
            os.close(slave)
        processes.append({"pid": process.pid, "argv": argv, "cwd": str(out), "worktree": str(Path.cwd()), "port": port})
        write_json(out / "processes.json", processes)
        offset = len(screen)
        trusted = False

        def started():
            nonlocal trusted
            if not trusted and b"Trust project folder?" in screen[offset:]:
                send(b"\x1b[B\x1b[B\r")
                trusted = True
            return any(e.get("pid") == process.pid and e["event"] == "start" for e in events())

        wait_for(started, "public main session start", 35)
        identity = next(e for e in reversed(events()) if e["event"] == "start")
        transcript = Path(identity["file"])
        wait_for(lambda: state().get("lease", {}).get("pid") == process.pid, "public main lease")
        assert len(children()) == 1, "expected one production news child"
        pump(0.8)

    def stop_pty(child, terminal, output):
        if child is None:
            return
        began = time.monotonic()
        alive = child.poll() is None
        # Drain shutdown rendering so a full PTY cannot block a graceful exit.
        try:
            os.killpg(child.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        while child.poll() is None and time.monotonic() - began < 3:
            ready, _, _ = select.select([terminal], [], [], 0.05)
            if ready:
                try:
                    output.extend(os.read(terminal, 65536))
                except OSError:
                    time.sleep(0.01)
        forced = child.poll() is None
        if forced:
            result["blockers"].append("TUI main required SIGKILL during cleanup")
            os.killpg(child.pid, signal.SIGKILL)
        child.wait(timeout=3)
        if alive:
            result.setdefault("shutdowns", []).append({"pid": child.pid,
                "seconds": time.monotonic() - began, "forced": forced})

    def stop_tui():
        nonlocal master
        if process is not None:
            children()
            stop_pty(process, master, screen)
        if master is not None:
            os.close(master)
            master = None

    def append_event(owner=None, question="Local PTY acceptance fixture", timestamp=None):
        event = {"v": 1, "ts": datetime.now(timezone.utc).isoformat(),
                 "owner": str(owner or out), "task": "t1", "type": "needs-human", "question": question}
        if timestamp:
            event["ts"] = timestamp
        with (out / "kit-state/events.jsonl").open("ab") as handle:
            fcntl.flock(handle, fcntl.LOCK_EX)
            handle.write((json.dumps(event) + "\n").encode())
            handle.flush()
            os.fsync(handle.fileno())

    def emit():
        append_event()
        wait_for(lambda: state().get("pending"), "pending batch")
        value = state()
        batch = value["pending"]["batchId"]
        write_json(out / (batch + "-prepared.json"), value)
        return batch

    def committed(batch):
        wait_for(lambda: state().get("lastAck", {}).get("batchId") == batch and not state().get("pending"), "durable receipt commit")
        assert len(entries(batch)) == 1, "delivery count across transcripts is not one"
        write_json(out / (batch + "-committed.json"), state())
        wait_for(lambda: snapshot().get("idle") is True and snapshot().get("queued") is False,
                 "agent idle", timeout=120 if live else 20)
        pump(0.6)

    try:
        if live:
            start()

            def assistant_messages():
                return [e["message"] for e in jsonl(transcript, live_log=True)
                        if e.get("message", {}).get("role") == "assistant" and
                        e["message"].get("stopReason") != "toolUse" and
                        (e["message"].get("stopReason") == "error" or
                         any(c.get("type") == "text" and c.get("text") for c in e["message"].get("content", [])))]

            def live_prompt(prompt):
                before = len(assistant_messages())
                command(prompt)
                wait_for(lambda: len(assistant_messages()) > before and snapshot().get("idle") is True,
                         "live model response", timeout=120)
                message = assistant_messages()[-1]
                assert message.get("stopReason") not in ("error", "aborted"), "live provider failed"
                assert message.get("model") == model_id and message.get("provider") == provider_name, "unexpected live model"
                return message

            prompt = ("Before starting work, explain how you would handle fixing a README typo and researching a queue design "
                      "with a written report. Determine this main session's exact owner by reading only HRK_MAIN_OWNER "
                      "from the environment and include OWNER=<exact value>; never print other variables or credentials. Read the main lifecycle commands reference "
                      "linked by your installed main skill. Name that reference path and the explicit resume command. "
                      "Do not launch a runner or change files yet. Keep the answer concise.")
            evidence = {"initial": live_prompt(prompt)}
            before = len(assistant_messages())
            batch = emit()
            committed(batch)
            wait_for(lambda: len(assistant_messages()) > before and snapshot().get("idle") is True,
                     "live news response", timeout=120)
            evidence["news"] = assistant_messages()[-1]
            result["checks"]["authenticated_news_reaches_transcript"] = True
            live_prompt("Remember these acceptance notes for compaction: " + "Preserve main routing and owner. " * 300)
            offset = len(events())
            command("/compact Preserve the main routing rules, fixed owner and commands reference.")
            wait_for(lambda: any(e["event"] == "session_compact" for e in events()[offset:]),
                     "live compaction", timeout=120)
            evidence["afterCompact"] = live_prompt(prompt)
            result["checks"]["authenticated_compaction_completed"] = True
            original = transcript
            stop_tui()
            start(original)
            assert transcript == original, "live explicit resume changed target"
            evidence["afterResume"] = live_prompt(prompt)
            result["checks"]["authenticated_explicit_resume_completed"] = True
            assert len(entries(batch)) == 1, "live compact/resume duplicated receipt"
            write_json(out / "live-responses.json", evidence)
            for stage in ("initial", "afterCompact", "afterResume"):
                answer = "\n".join(c.get("text", "") for c in evidence[stage]["content"] if c.get("type") == "text")
                assert "OWNER=" + str(out) in answer, "live model did not identify fixed owner at " + stage
                assert "references/commands.md" in answer, "live model omitted commands reference at " + stage
            result["checks"]["live_owner_and_reference_survive_compact_resume"] = True
            result["semanticReview"] = "Review live-responses.json for routing, owner and reference accuracy."
            return 0
        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
        worker = threading.Thread(target=server.serve_forever)
        worker.start()
        start()
        result["checks"]["public_launch_registers_runtime"] = True
        assert transcript.stat().st_mode & 0o777 == 0o600
        result["checks"]["private_transcript"] = True
        committed(emit())
        result["checks"]["fresh_idle_delivery_without_input"] = True
        child, = children()

        def cpu_seconds():
            value = subprocess.check_output(["ps", "-p", str(child), "-o", "time="], text=True).strip()
            return sum(float(part) * 60 ** i for i, part in enumerate(reversed(value.split(":"))))

        before_cpu = cpu_seconds()
        before_requests = len(jsonl(out / "provider.jsonl", live_log=True))
        pump(2.5)
        delta_cpu = cpu_seconds() - before_cpu
        assert delta_cpu <= 1, "idle news child spun without tasks"
        assert len(jsonl(out / "provider.jsonl", live_log=True)) == before_requests, "idle news child woke provider"
        write_json(out / "idle-evidence.json", {"childPID": child, "elapsedSeconds": 2.5, "cpuSeconds": delta_cpu})
        result["checks"]["idle_without_tasks_does_not_spin_or_wake_model"] = True
        # A second real main must not steal the lease, even after its child dies.
        send(b"LEASE_OWNER_DRAFT")
        editor("LEASE_OWNER_DRAFT")
        batch = emit()
        competing_out = out / "competitor"
        competing_out.mkdir()
        competing_master, competing_slave = pty.openpty()
        competitor = None
        competing_screen = bytearray()
        conflict_offset = 0
        conflict_text = b"already waiting"

        def start_competitor(cwd=out):
            rival = subprocess.Popen(argv, cwd=cwd, env=dict(env, HRK_PROBE_OUT=str(competing_out)),
                                     stdin=competing_slave, stdout=competing_slave, stderr=competing_slave,
                                     start_new_session=True)
            processes.append({"pid": rival.pid, "argv": argv, "cwd": str(cwd), "worktree": str(Path.cwd())})
            write_json(out / "processes.json", processes)
            return rival

        def stop_competitor():
            stop_pty(competitor, competing_master, competing_screen)

        try:
            fcntl.ioctl(competing_slave, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 140, 0, 0))
            argv = [str(args.kit_bin.resolve()), "main", "--agent", args.harness + ":hrk-probe/probe:off",
                    "--exec", "--resume", str(transcript)]
            competitor = start_competitor()

            def conflict():
                children(competitor.pid)
                ready, _, _ = select.select([competing_master], [], [], 0)
                if ready:
                    competing_screen.extend(os.read(competing_master, 65536))
                return conflict_text in competing_screen[conflict_offset:]

            wait_for(conflict, "competing main lease conflict")
            assert state()["lease"]["pid"] == process.pid and not entries(batch), "competing main stole delivery"
            stop_competitor()
            child, = children()
            os.kill(process.pid, signal.SIGSTOP)
            try:
                conflict()
                conflict_offset = len(competing_screen)
                conflict_text = b"owns the lease"
                os.kill(child, signal.SIGKILL)
                wait_for(lambda: not process_alive(child), "own child exits before competing retry")
                competitor = start_competitor()
                wait_for(conflict, "competing main remains fenced after owner child death")
                assert state()["lease"]["pid"] == process.pid and not entries(batch), "child death released living parent's lease"
            finally:
                os.kill(process.pid, signal.SIGCONT)
            result["checks"]["two_main_ptys_cannot_share_owner_after_child_death"] = True
            stop_competitor()
            argv = argv[:-2]
            competitor = start_competitor(competing_out)
            other_path = out / "kit-state" / ("main-news-" + hashlib.sha256(str(competing_out).encode()).hexdigest()[:12] + ".json")

            def independent_ready():
                conflict()
                return other_path.exists() and json.loads(other_path.read_text()).get("lease", {}).get("pid") == competitor.pid

            wait_for(independent_ready, "independent owner lease")
            append_event(competing_out, "Independent owner acceptance news")

            def independent_committed():
                conflict()
                return json.loads(other_path.read_text()).get("lastAck")

            wait_for(independent_committed, "independent owner receipt")
            other = json.loads(other_path.read_text())
            assert other["lastAck"]["target"]["owner"] == str(competing_out)
            assert other["lastAck"]["batchId"] != batch and not entries(batch), "independent owner consumed held batch"
            assert state()["pending"]["batchId"] == batch, "independent owner advanced original pending"
            write_json(out / "independent-owner-evidence.json", {"original": state(), "other": other})
            result["checks"]["different_owner_ptys_deliver_independently"] = True
        finally:
            stop_competitor()
            os.close(competing_slave)
            os.close(competing_master)
            (competing_out / "screen.txt").write_bytes(competing_screen)
        send(b"\x15")
        editor("")
        committed(batch)
        result["checks"]["own_child_restart_preserves_pending_once"] = True
        for label, draft in [("normal", "UNSENT_DRAFT"), ("whitespace", "   ")]:
            send(draft.encode())
            editor(draft)
            batch = emit()
            pump(1)
            assert snapshot()["editor"] == draft and not entries(batch), "news changed or bypassed draft"
            send(b"\x15")
            editor("")
            committed(batch)
            result["checks"][label + "_draft_preserved"] = True
        for delay in (0.45, 0.5):
            send(b"BEFORE_DRAIN_TICK")
            editor("BEFORE_DRAIN_TICK")
            batch = emit()
            send(b"\x15")
            pump(delay)
            send(b"DRAIN_TICK_DRAFT")
            editor("DRAIN_TICK_DRAFT")
            already_appended = bool(entries(batch))
            pump(0.8)
            assert snapshot()["editor"] == "DRAIN_TICK_DRAFT", "drain tick lost terminal input"
            if not already_appended:
                assert not entries(batch), "drain appended after observing a new draft"
            send(b"\x15")
            editor("")
            committed(batch)
        result["checks"]["terminal_input_at_drain_tick_preserves_draft"] = True
        # A paste remains incomplete across multiple drain ticks.
        send(b"\x1b[20")
        pump(0.01)
        send(b"0~first line\n")
        batch = emit()
        pump(0.3)
        premature = bool(entries(batch))
        send(b"second ")
        pump(0.3)
        premature |= bool(entries(batch))
        send(b"line\x1b[20")
        pump(0.01)
        premature |= bool(entries(batch))
        send(b"1~")
        editor("first line\nsecond line")
        write_json(out / "paste-evidence.json", {"batchId": batch, "injectedBeforePasteEnd": premature,
                                                "editorAfterPaste": snapshot(), "entries": entries(batch)})
        assert not premature, "news injected inside bracketed paste"
        pump(0.7)
        assert not entries(batch) and snapshot()["editor"] == "first line\nsecond line"
        send(b"\x15\x7f\x15")
        editor("")
        committed(batch)
        result["checks"]["chunked_multiline_paste_preserved"] = True
        # Even Omp's native paste timeout cannot replace an observed raw END.
        warning_start = len(screen)
        requests_before_warning = len(jsonl(out / "provider.jsonl", live_log=True))
        send(b"\x1b[200~")
        batch = emit()
        pump(10.3)
        assert not entries(batch), "unterminated paste timeout released news"
        assert b"Paste incomplete or unknown" in screen[warning_start:], "missing paste recovery warning"
        assert len(jsonl(out / "provider.jsonl", live_log=True)) == requests_before_warning, "paste warning woke the model"
        editor("")
        send(b"\x1b[201~")
        editor("")
        committed(batch)
        result["checks"]["unterminated_paste_warns_and_waits_for_real_end"] = True
        for _ in range(3):
            reload_command = "/reload" if args.harness == "pi" else "/reload-plugins"
            send(reload_command.encode())
            editor(reload_command)
            batch = emit()
            prepared = state()["pending"]
            pump(0.7)
            assert not entries(batch), "reload command draft bypassed input gate"
            old_child, = children()
            reload_screen = len(screen)
            send(b"\r")
            notice = b"Reloaded keybindings, extensions" if args.harness == "pi" else b"Plugins reloaded."
            wait_for(lambda: notice in screen[reload_screen:], "runtime reload confirmation")
            if args.harness == "pi":
                wait_for(lambda: len(children()) == 1 and old_child not in children(), "reloaded extension replaces child")
            # A reload gap is unknown until a real END re-establishes input state.
            send(b"\x1b[200~\x1b[201~")
            editor("")
            committed(batch)
            assert state()["lastAck"]["digest"] == prepared["digest"], "reload changed pending payload"
            assert len(children()) == 1, "reload duplicated the news child"
            assert sum(process_alive(pid) for pid in bridges) == 1, "reload leaked an old child"
        result["checks"]["repeated_reload_preserves_never_sent_batch_once"] = True
        command("/settings")
        pump(0.5)
        batch = emit()
        pump(1)
        assert not entries(batch), "news injected into settings selector"
        send(b"\x1b")
        committed(batch)
        result["checks"]["modal_holds_until_escape"] = True
        for label, draft in [("draft", b"resume draft"), ("unterminated_paste", b"\x1b[200~")]:
            send(draft)
            if label == "draft":
                editor("resume draft")
            batch = emit()
            pump(1.2)
            assert not entries(batch), "news bypassed input before exit/resume"
            original = transcript
            child, = children()
            process.kill()
            process.wait(timeout=3)
            deadline = time.monotonic() + 7
            while process_alive(child) and time.monotonic() < deadline:
                pump(0.05)
            assert not process_alive(child), "bridge survived parent SIGKILL"
            result["checks"]["parent_sigkill_child_cleanup"] = True
            stop_tui()
            start(original)
            assert transcript == original, "explicit resume changed transcript"
            committed(batch)
            result["checks"]["public_explicit_resume_" + label + "_pending_once"] = True
        previous = next(e for e in reversed(events()) if e["event"] == "start")
        send(b"/new")
        editor("/new")
        batch = emit()
        prepared = state()["pending"]
        pump(0.7)
        assert not entries(batch), "session command draft bypassed input gate"
        transition_offset = len(events())
        send(b"\r")
        committed(batch)
        assert all(e.get("id", previous["id"]) == previous["id"] for e in events()[transition_offset:]), "pending news switched session"
        assert state()["lastAck"]["target"] == prepared["target"], "blocked transition changed delivery target"
        result["checks"]["new_session_blocked_until_pending_committed"] = True
        command("/new")
        wait_for(lambda: snapshot().get("id") not in (None, previous["id"]), "new session identity")
        identity = snapshot()
        transcript = Path(identity["file"])
        write_json(out / "new-session.json", identity)
        prior_ack = state()["lastAck"]["batchId"]
        before_event = (out / "kit-state/events.jsonl").stat().st_size
        append_event(question="NEWS_BEFORE_NEW_SESSION_HEADER")
        pump(0.8)
        if not transcript.exists():
            cursor = out / "kit-state" / ("cursor-" + state()["cursor"])
            assert int(cursor.read_text()) <= before_event, "absent session header advanced cursor"
        write_json(out / "new-session-before-turn.json", {"runtime": snapshot(), "delivery": state(),
                                                        "headerInitiallyExists": identity["fileExists"]})
        command("Initialize the fresh local acceptance session.")
        wait_for(lambda: transcript.exists() and snapshot().get("idle") is True, "new session first turn")
        send(b"\x1b[200~\x1b[201~")
        editor("")
        wait_for(lambda: state().get("lastAck", {}).get("batchId") != prior_ack and not state().get("pending"),
                 "pre-header news survives session initialization")
        initialized_batch = state()["lastAck"]["batchId"]
        committed(initialized_batch)
        assert "NEWS_BEFORE_NEW_SESSION_HEADER" in json.dumps(entries(initialized_batch)), "initialization lost queued news"
        assert state()["lastAck"]["target"]["sessionId"] == identity["id"], "new session retained old delivery identity"
        result["checks"]["new_session_materializes_and_receives_news"] = True
        provider_started.clear()
        provider_release.clear()
        command("Hold the local response while I queue another prompt.")
        wait_for(provider_started.is_set, "busy provider request")
        batch = emit()
        command("QUEUED_USER_PROMPT")
        wait_for(lambda: snapshot().get("queued") is True, "queued user input")
        send(b"DRAFT_DURING_BUSY")
        editor("DRAFT_DURING_BUSY")
        pump(0.7)
        assert not entries(batch), "news entered the agent queue while busy"
        provider_release.set()
        wait_for(lambda: snapshot().get("idle") is True and snapshot().get("queued") is False, "queued prompt finishes")
        pump(0.7)
        assert not entries(batch) and snapshot()["editor"] == "DRAFT_DURING_BUSY", "settled turn bypassed draft"
        history = jsonl(transcript)
        queued = next(i for i, e in enumerate(history) if e.get("message", {}).get("role") == "user" and
                      "QUEUED_USER_PROMPT" in json.dumps(e["message"].get("content")))
        send(b"\x15")
        editor("")
        committed(batch)
        history = jsonl(transcript)
        receipt = next(i for i, e in enumerate(history) if e.get("details", {}).get("batchId") == batch)
        assert queued < receipt, "news overtook queued user input"
        result["checks"]["busy_queue_and_later_draft_precede_news"] = True
        offset = len(events())
        provider_tool = True
        command("Run the local acceptance hold tool.")
        wait_for(lambda: any(e["event"] == "probe_tool_start" for e in events()[offset:]), "real tool starts")
        batch = emit()
        pump(0.8)
        assert not entries(batch), "news entered the runtime during tool execution"
        assert snapshot().get("idle") is False, "running tool appeared idle"
        (out / "release-tool").touch()
        wait_for(lambda: any(e["event"] == "probe_tool_end" for e in events()[offset:]), "real tool finishes")
        committed(batch)
        result["checks"]["busy_tool_finishes_before_news"] = True
        requests_before_retry = len(jsonl(out / "provider.jsonl", live_log=True))
        provider_started.clear()
        provider_release.clear()
        provider_status = 503
        command("Retry this local provider response before delivering news.")
        wait_for(provider_started.is_set, "retryable provider request")
        batch = emit()
        pump(0.8)
        assert not entries(batch), "news entered the runtime before retry"
        provider_status = 200
        provider_release.set()
        wait_for(lambda: len(jsonl(out / "provider.jsonl", live_log=True)) >= requests_before_retry + 2,
                 "runtime retries provider request")
        requests = jsonl(out / "provider.jsonl", live_log=True)[requests_before_retry:]
        assert requests[0]["messages"] == requests[1]["messages"], "next request was not a retry of the failed turn"
        committed(batch)
        result["checks"]["provider_retry_finishes_before_news"] = True
        for outcome in ("success", "cancel", "failure"):
            provider_started.clear()
            command("Compaction history " + "padding " * 1500)
            wait_for(provider_started.is_set, "compaction history provider request")
            wait_for(lambda: snapshot().get("idle") is True, "compaction history settles")
            pump(0.6)
            offset = len(events())
            screen_offset = len(screen)
            provider_started.clear()
            provider_release.clear()
            provider_status = 400 if outcome == "failure" else 200
            command("/compact Preserve the local acceptance history.")
            wait_for(provider_started.is_set, "manual compaction provider request")
            assert any(e["event"] == "session_before_compact" for e in events()[offset:]), "missing compaction lifecycle"
            batch = emit()
            pump(0.8)
            assert not entries(batch), "news entered the runtime during manual compaction"
            if outcome == "cancel":
                send(b"\x1b")
                wait_for(lambda: any(e["event"] == "compaction_aborted" for e in events()[offset:]),
                         "manual compaction cancellation")
            provider_status = 200
            provider_release.set()
            if outcome == "success":
                wait_for(lambda: any(e["event"] == "session_compact" for e in events()[offset:]), "manual compaction completion")
            elif outcome == "failure":
                wait_for(lambda: b"Local acceptance provider failure" in screen[screen_offset:], "manual compaction failure notice")
            committed(batch)
            result["checks"]["manual_compaction_" + outcome + "_releases_news_once"] = True
        for outcome in ("success", "cancel", "failure"):
            # A cancelled compaction retains high usage; isolate the next outcome
            # so its pre-prompt maintenance cannot consume the forced usage reply.
            previous_id = snapshot()["id"]
            command("/new")
            wait_for(lambda: snapshot().get("id") not in (None, previous_id), "automatic compaction fresh session")
            provider_started.clear()
            command("Initialize automatic compaction history " + "padding " * 1500)
            wait_for(provider_started.is_set, "automatic compaction initial history")
            wait_for(lambda: snapshot().get("idle") is True, "automatic compaction initial turn settles")
            send(b"\x1b[200~\x1b[201~")
            editor("")
            pump(0.6)
            offset = len(events())
            provider_started.clear()
            provider_usage = 31900
            compaction_status = 400 if outcome == "failure" else 200
            command("Automatic compaction history " + "padding " * 1500)
            wait_for(lambda: any(e["event"] == "session_before_compact" and e.get("reason") != "manual"
                                 for e in events()[offset:]), "automatic compaction starts")
            wait_for(provider_started.is_set, "automatic compaction provider request")
            batch = emit()
            pump(0.8)
            assert not entries(batch), "news entered the runtime during automatic compaction"
            if outcome == "cancel":
                send(b"\x1b")
                wait_for(lambda: any(e["event"] == "compaction_aborted" for e in events()[offset:]),
                         "automatic compaction cancellation")
            provider_status = 200
            provider_release.set()
            if outcome == "success":
                wait_for(lambda: any(e["event"] == "session_compact" for e in events()[offset:]), "automatic compaction completion")
            elif outcome == "failure":
                wait_for(lambda: any(e["event"] in ("auto_compaction_end", "session_compact_failed") and
                                     "Local acceptance provider failure" in e.get("errorMessage", "")
                                     for e in events()[offset:]), "automatic compaction failure event")
            committed(batch)
            result["checks"]["automatic_compaction_" + outcome + "_releases_news_once"] = True
        screen_offset = len(screen)
        command("Handled acceptance input")
        wait_for(lambda: any(e["event"] == "input_handled" for e in events()), "input handled without agent")
        editor("")
        requests_before_warning = len(jsonl(out / "provider.jsonl", live_log=True))
        batch = emit()
        pump(8)
        assert b"News still paused." not in screen[screen_offset:], "recovery warning appeared before threshold"
        wait_for(lambda: b"News still paused." in screen[screen_offset:], "pending news recovery warning", timeout=5)
        assert not entries(batch) and state().get("pending", {}).get("batchId") == batch, "warning opened delivery gate"
        assert len(jsonl(out / "provider.jsonl", live_log=True)) == requests_before_warning, "warning called provider"
        assert snapshot().get("idle") is True and snapshot().get("queued") is False and snapshot().get("editor") == ""
        result["checks"]["blocked_input_warns_without_delivery"] = True
        reload_screen = len(screen)
        command("/reload" if args.harness == "pi" else "/reload-plugins")
        notice = b"Reloaded keybindings, extensions" if args.harness == "pi" else b"Plugins reloaded."
        wait_for(lambda: notice in screen[reload_screen:], "explicit recovery reload")
        send(b"\x1b[200~\x1b[201~")
        editor("")
        committed(batch)
        result["checks"]["explicit_reload_recovers_blocked_input_once"] = True
        previous_id = snapshot()["id"]
        send(b"/probe-branch")
        editor("/probe-branch")
        batch = emit()
        prepared = state()["pending"]
        assert not entries(batch), "branch command draft bypassed input gate"
        offset = len(events())
        send(b"\r")
        wait_for(lambda: any(e["event"] == "branch_result" for e in events()[offset:]), "runtime branch result")
        branch = next(e for e in events()[offset:] if e["event"] == "branch_result")
        pump(0.8)
        write_json(out / "pending-branch-evidence.json", {"before": prepared, "branch": branch,
                                                        "after": state(), "runtime": snapshot(), "receipts": entries(batch)})
        assert branch.get("cancelled") is True and branch["id"] == previous_id, "pending news allowed a branch transition"
        committed(batch)
        assert state()["lastAck"]["target"] == prepared["target"], "blocked branch changed delivery target"
        result["checks"]["branch_blocked_until_pending_committed"] = True
        command("Branch after the committed acceptance news.")
        wait_for(lambda: snapshot().get("idle") is True and snapshot().get("editor") == "", "branch history settles")
        pump(0.8)
        offset = len(events())
        command("/probe-branch")
        wait_for(lambda: any(e["event"] == "branch_result" for e in events()[offset:]), "permitted branch result")
        branch = next(e for e in events()[offset:] if e["event"] == "branch_result")
        assert branch.get("cancelled") is False and branch["id"] != previous_id, "committed news prevented branching"
        wait_for(lambda: snapshot().get("id") == branch["id"], "branched session observation")
        transcript = Path(branch["file"])
        send(b"\x15\x1b[200~\x1b[201~")
        editor("")
        command("Initialize the branched acceptance session.")
        wait_for(lambda: snapshot().get("idle") is True and transcript.exists(), "branched session materializes")
        pump(0.8)
        assert len(entries(batch)) == 1, "branch copied history became a second delivery"
        branched_batch = emit()
        committed(branched_batch)
        assert state()["lastAck"]["target"]["sessionId"] == branch["id"], "branch retained old delivery identity"
        assert state()["lastAck"]["target"]["sessionFile"] == str(transcript), "branch retained old transcript"
        write_json(out / "committed-branch-evidence.json", {"branch": branch, "delivery": state(),
                                                          "originalReceipts": entries(batch), "newReceipts": entries(branched_batch)})
        result["checks"]["committed_branch_updates_identity_without_redelivery"] = True
        original = Path(prepared["target"]["sessionFile"])
        (out / "resume-target.txt").write_text(str(original))
        send(b"/probe-resume")
        editor("/probe-resume")
        batch = emit()
        offset = len(events())
        send(b"\r")
        wait_for(lambda: any(e["event"] == "switch_result" for e in events()[offset:]), "pending session switch result")
        switched = next(e for e in events()[offset:] if e["event"] == "switch_result")
        assert switched.get("cancelled") is True and switched["id"] == branch["id"], "pending news allowed session switch"
        committed(batch)
        result["checks"]["resume_switch_blocked_until_pending_committed"] = True
        offset = len(events())
        command("/probe-resume")
        wait_for(lambda: any(e["event"] == "switch_result" for e in events()[offset:]), "committed session switch result")
        switched = next(e for e in events()[offset:] if e["event"] == "switch_result")
        assert switched.get("cancelled") is False and switched["id"] == previous_id, "committed news prevented session switch"
        wait_for(lambda: snapshot().get("id") == previous_id, "resumed session observation")
        transcript = original
        send(b"\x1b[200~\x1b[201~")
        editor("")
        committed(emit())
        assert state()["lastAck"]["target"]["sessionId"] == previous_id, "switch retained branch identity"
        result["checks"]["committed_resume_updates_delivery_identity"] = True
        for picker in ("/resume", "/tree"):
            command(picker)
            pump(0.5)
            batch = emit()
            pump(0.8)
            assert not entries(batch), "news entered the " + picker + " picker"
            send(b"\x1b")
            committed(batch)
            result["checks"][picker[1:] + "_picker_holds_news_until_escape"] = True
        # Hold the event writer lock after preparation to stop commit, not append.
        send(b"ACK_BOUNDARY_DRAFT")
        editor("ACK_BOUNDARY_DRAFT")
        batch = emit()
        prepared = state()["pending"]
        with (out / "kit-state/events.jsonl").open("ab") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            send(b"\x15")
            editor("")
            wait_for(lambda: len(entries(batch)) == 1, "physical receipt before blocked commit")
            wait_for(lambda: snapshot().get("idle") is True, "unacknowledged news turn settles")
            assert state().get("pending", {}).get("batchId") == batch, "writer lock did not hold commit"
            old_child, = children()
            os.kill(old_child, signal.SIGKILL)
            reload_screen = len(screen)
            command("/reload" if args.harness == "pi" else "/reload-plugins")
            notice = b"Reloaded keybindings, extensions" if args.harness == "pi" else b"Plugins reloaded."
            wait_for(lambda: notice in screen[reload_screen:], "persisted receipt reload confirmation")
            send(b"\x1b[200~\x1b[201~")
            editor("")
            pump(1)
            assert len(entries(batch)) == 1, "reload appended an acknowledged-on-disk batch again"
            assert state()["pending"] == prepared, "reload mutated unresolved persisted batch"
            fcntl.flock(lock, fcntl.LOCK_UN)
        committed(batch)
        assert state()["lastAck"]["target"] == prepared["target"], "reload moved receipt target"
        assert len(children()) == 1, "receipt recovery duplicated the news child"
        result["checks"]["reload_recovers_persisted_receipt_after_child_kill"] = True
        project_dir = out / "attached-project"
        project_dir.mkdir()
        subprocess.run(["git", "init", "-q", str(project_dir)], env=env, check=True, capture_output=True)
        shell_dir = out / "different-shell-cwd"
        shell_dir.mkdir()

        def kit(*arguments):
            response = subprocess.run([str(args.kit_bin.resolve()), *arguments], cwd=shell_dir,
                                      env=dict(env, HRK_MAIN_OWNER=str(out)), capture_output=True, text=True, timeout=15)
            with (out / "commands.jsonl").open("a") as log:
                log.write(json.dumps({"argv": list(arguments), "cwd": str(shell_dir), "code": response.returncode,
                                      "stdout": response.stdout, "stderr": response.stderr}) + "\n")
            assert response.returncode == 0, "scratch kit command failed: " + response.stderr

        kit("project", "register", str(project_dir), "--name", "attached")
        send(b"ATTACHMENT_DRAFT")
        editor("ATTACHMENT_DRAFT")
        append_event("project:attached", "OLD_ATTACHMENT_NEWS", "2000-01-01T00:00:00Z")
        kit("main", "--agent", args.harness, "--project", "attached")
        append_event("project:unattached", "UNRELATED_PROJECT_NEWS")
        append_event("project:attached", "NEW_ATTACHMENT_NEWS")
        wait_for(lambda: state().get("pending"), "attached project reaches existing waiter")
        prepared = state()["pending"]
        batch = prepared["batchId"]
        assert len(prepared["lines"]) == 1 and "NEW_ATTACHMENT_NEWS" in prepared["lines"][0], "attachment cutoff or scope failed"
        kit("main", "--agent", args.harness, "--no-projects")
        kit("project", "forget", "attached", "--yes")
        pump(0.8)
        assert state()["pending"] == prepared and not entries(batch), "detach or registry change mutated pending"
        send(b"\x15")
        editor("")
        committed(batch)
        append_event("project:attached", "DETACHED_PROJECT_NEWS")
        pump(2.2)
        assert not state().get("pending"), "detached project still delivered news"
        committed(emit())
        assert "DETACHED_PROJECT_NEWS" not in json.dumps(entries(state()["lastAck"]["batchId"])), "detached news leaked into owner batch"
        assert state()["lastAck"]["target"]["owner"] == str(out), "shell cwd changed fixed owner"
        result["checks"]["attachment_cutoff_detach_registry_and_shell_cwd"] = True
        def recover_transition(marker):
            command("/reload" if args.harness == "pi" else "/reload-plugins")
            pump(1)
            send(b"\x1b[200~\x1b[201~")
            editor("")
            wait_for(lambda: state().get("pending") or marker in json.dumps(entries(state()["lastAck"]["batchId"])),
                     "explicit reload reconnects after cancelled transition")
            recovered = state().get("pending", {}).get("batchId") or state()["lastAck"]["batchId"]
            committed(recovered)
            assert marker in json.dumps(entries(recovered))

        for operation in (("switch", "branch", "tree") if args.harness == "omp" else ("switch", "branch")):
            previous_id = snapshot()["id"]
            offset = len(events())
            screen_offset = len(screen)
            command("/probe-veto " + operation)
            wait_for(lambda: any(e["event"] == "later_transition_veto" for e in events()[offset:]), "later hook veto")
            veto = next(e for e in events()[offset:] if e["event"] == "later_transition_veto")
            assert veto.get("cancelled") is True and snapshot()["id"] == previous_id
            before_requests = len(jsonl(out / "provider.jsonl", live_log=True))
            marker = "LATER_VETO_RECOVERY_" + operation
            append_event(question=marker)
            pump(10.3)
            assert b"News paused for session change" in screen[screen_offset:], "cancelled transition lacked explicit recovery guidance"
            assert len(jsonl(out / "provider.jsonl", live_log=True)) == before_requests, "later veto woke model"
            recover_transition(marker)
            result["checks"]["later_" + operation + "_veto_stays_closed_until_explicit_reload"] = True
        if args.harness == "omp":
            for outcome in ("cancel", "failure"):
                command("Tree summarization history " + "padding " * 1500)
                wait_for(lambda: snapshot().get("idle") is True and snapshot().get("editor") == "", "tree history settles")
                pump(0.8)
                screen_offset = len(screen)
                previous_id = snapshot()["id"]
                provider_started.clear()
                provider_release.clear()
                provider_status = 400 if outcome == "failure" else 200
                command("/tree")
                pump(0.5)
                send(b"\x1b[A\x1b[13;2~")
                wait_for(provider_started.is_set, "real tree summarization request")
                assert b"Summarizing branch" in screen[screen_offset:], "tree summary bypassed the native cancellation UI"
                marker = "TREE_SUMMARY_RECOVERY_" + outcome
                append_event(question=marker)
                pump(0.8)
                if outcome == "cancel":
                    send(b"\x1b")
                    pump(0.2)
                provider_status = 200
                provider_release.set()
                notice = b"Branch summarization cancelled" if outcome == "cancel" else b"Local acceptance provider failure"
                wait_for(lambda: notice in screen[screen_offset:], "native tree summary " + outcome)
                if outcome == "cancel":
                    pump(0.2)
                    send(b"\x1b")
                editor("")
                assert snapshot()["id"] == previous_id, "cancelled tree changed session"
                assert b"News paused for session change" in screen[screen_offset:], "tree failure lacked recovery guidance"
                before_requests = len(jsonl(out / "provider.jsonl", live_log=True))
                pump(1.2)
                assert len(jsonl(out / "provider.jsonl", live_log=True)) == before_requests, "tree failure woke model before recovery"
                paths = {Path(e["file"]) for e in events() if e.get("file")}
                assert all(marker not in str(e.get("content", "")) for path in paths
                           for e in jsonl(path, live_log=True) if e.get("customType") == "hrk-news"), "tree failure delivered before reload"
                recover_transition(marker)
                result["checks"]["tree_summary_" + outcome + "_explicit_reload_recovery"] = True
        command("Extension compaction history " + "padding " * 1500)
        wait_for(lambda: snapshot().get("idle") is True and snapshot().get("editor") == "", "extension compaction history settles")
        pump(0.8)
        offset = len(events())
        screen_offset = len(screen)
        provider_status = 400
        command("/probe-compact")
        wait_for(lambda: any(e["event"] == "extension_compact_error" for e in events()[offset:]), "extension compaction failure")
        provider_status = 200
        batch = emit()
        if args.harness == "omp":
            assert b"Error: Compaction failed:" in screen[screen_offset:], "extension compaction failure lacked TUI settlement"
        committed(batch)
        result["checks"]["extension_compaction_failure_recovers_without_duplicate"] = True
        if args.harness == "omp":
            stop_tui()
            config_path = out / "omp.yml"
            configured_settings = config_path.read_text()
            config_path.write_text(configured_settings.replace("setupVersion: 2", "setupVersion: 0"))
            screen_offset = len(screen)
            start()
            original = transcript
            batch = emit()
            pump(1)
            if not entries(batch):
                assert any(title in screen[screen_offset:] for title in
                           (b"Choose composer shape", b"Choose your default model", b"Set up your providers", b"Pick a theme")), "fresh setup neither delivered nor showed onboarding"
                stop_tui()
                config_path.write_text(configured_settings)
                start(original)
                committed(batch)
                result["checks"]["setup_modal_holds_news_until_explicit_resume"] = True
            else:
                committed(batch)
                result["checks"]["public_reserved_resume_skips_first_run_setup"] = True
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
        write_json(out / "failure-state.json", {"delivery": state(), "runtime": snapshot()})
        if isinstance(error, InterruptedError):
            raise
    finally:
        provider_release.set()
        stop_tui()
        if server is not None:
            server.shutdown()
            server.server_close()
        if worker is not None:
            worker.join()
        deadline = time.monotonic() + 7
        while any(process_alive(pid) for pid in bridges) and time.monotonic() < deadline:
            time.sleep(0.05)
        remaining = [pid for pid in bridges if process_alive(pid)]
        if remaining:
            result["blockers"].append("news children required SIGKILL during cleanup")
            for pid in remaining:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            deadline = time.monotonic() + 2
            while any(process_alive(pid) for pid in remaining) and time.monotonic() < deadline:
                time.sleep(0.05)
        result["checks"]["owned_processes_exited"] = all(not process_alive(p["pid"]) for p in processes) and all(not process_alive(pid) for pid in bridges)
        result["bridgePIDs"] = sorted(bridges)
        (out / "screen.txt").write_bytes(screen)
        write_json(out / "result.json", result)
        if live and (result["blockers"] or not result["checks"]["owned_processes_exited"]):
            raise RuntimeError("live probe cleanup failed")
    print(json.dumps({"harness": args.harness, "version": result.get("version"), "checks": result["checks"],
                      "blockers": result["blockers"], "error": result.get("error"), "out": str(out)}))
    return 1 if result.get("error") or result["blockers"] or any(v is not True for v in result["checks"].values()) else 0


def main():
    signal.signal(signal.SIGTERM, interrupt)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=["all", "transcript-contract", "tui", "live"], default="all")
    parser.add_argument("--harness", choices=["pi", "omp"], required=True)
    parser.add_argument("--kit-bin", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--live-model", help="configured provider/model for the live case")
    parser.add_argument("--live-provider-config", type=Path, help="trusted Pi-format providers JSON; uses its configured credential command")
    args = parser.parse_args()
    if args.case == "transcript-contract":
        return run(args)
    if args.case == "live" and not all((args.live_model, args.live_provider_config)):
        parser.error("live requires --live-model and --live-provider-config")
    if args.case in ("tui", "live"):
        return run_tui(args)
    args.out.mkdir(parents=True, exist_ok=False)
    outcomes = {}
    for name, check in [("transcript-contract", run), ("tui", run_tui)]:
        case = argparse.Namespace(**vars(args))
        case.out = args.out / name
        outcomes[name] = check(case)
    write_json(args.out / "result.json", outcomes)
    return int(any(outcomes.values()))


if __name__ == "__main__":
    raise SystemExit(main())
