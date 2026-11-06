#!/usr/bin/env python3
"""Probe installed main runtimes with isolated profiles and a loopback provider.

No SDK mocks, global extensions, user credentials, or user state are used.
The runtime writes its own header into a launcher-allocated empty path. Its
probe extension starts the real news child; receipts must precede cursor commit.
A failed or incomplete gate exits nonzero and retains evidence under --out.
"""

import argparse
import fcntl
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
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
                process.kill()
                process.wait(timeout=3)
    reader.join()
    process.stdout.close()


def process_alive(pid):
    result = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, text=True, check=False)
    return bool(result.stdout.strip()) and not result.stdout.strip().startswith("Z")


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
    finally:
        provider_release.set()
        if process is not None and reader is not None:
            stop(process, reader)
        for event in events():
            if event["event"] != "bridge_start" or not process_alive(event["pid"]):
                continue
            try:
                os.kill(event["pid"], 15)
                deadline = time.monotonic() + 2
                while process_alive(event["pid"]) and time.monotonic() < deadline:
                    time.sleep(0.02)
                if process_alive(event["pid"]):
                    os.kill(event["pid"], 9)
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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--case", choices=["transcript-contract"], required=True)
    parser.add_argument("--harness", choices=["pi", "omp"], required=True)
    parser.add_argument("--kit-bin", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    return run(parser.parse_args())


if __name__ == "__main__":
    raise SystemExit(main())
