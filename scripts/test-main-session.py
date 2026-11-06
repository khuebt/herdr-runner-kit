#!/usr/bin/env python3
"""Probe installed main runtimes with isolated profiles and a loopback provider.

No SDK mocks, global extensions, user credentials, or user state are used.
This first-stage gate does not launch the news child: its runtime prerequisites
must be established first. The required --kit-bin names the scratch build for the
later bridge probe. An incomplete gate exits nonzero and retains evidence under
--out, even when the proposed empty-file bootstrap experiment succeeds.
"""

import argparse
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
const root = process.env.HRK_PROBE_OUT;
const log = (data) => appendFileSync(root + "/extension.jsonl", JSON.stringify({at:Date.now(),...data}) + "\n");
export default function(pi) {
  pi.registerProvider("hrk-probe", {
    baseUrl: process.env.HRK_PROBE_URL, apiKey: "local-fixture", api: "openai-completions",
    models: [{id:"probe",name:"Local transcript probe",reasoning:false,input:["text"],
      cost:{input:0,output:0,cacheRead:0,cacheWrite:0},contextWindow:32000,maxTokens:2048}]
  });
  let timer;
  pi.on("session_start", (_event, ctx) => {
    clearInterval(timer);
    log({event:"session_start", id:ctx.sessionManager.getSessionId(),
      file:ctx.sessionManager.getSessionFile(), cwd:ctx.sessionManager.getCwd()});
    timer = setInterval(async () => {
      const file = root + "/inbox.json";
      if (!existsSync(file)) return;
      const action = JSON.parse(readFileSync(file, "utf8"));
      unlinkSync(file);
      if (action.kind === "snapshot") {
        log({event:"snapshot",name:action.name,idle:ctx.isIdle(),entries:ctx.sessionManager.getEntries()});
      } else if (action.kind === "abort") {
        await ctx.abort();
        log({event:"abort_returned",idle:ctx.isIdle()});
      } else {
        pi.sendMessage({customType:"hrk-news",content:action.content,display:true,
          details:{batchId:action.batchId,owner:process.env.HRK_MAIN_OWNER,digest:action.digest}},
          {triggerTurn:action.triggerTurn,deliverAs:"followUp"});
        log({event:"send_returned",batchId:action.batchId,idle:ctx.isIdle()});
      }
    }, 20);
  });
  for (const name of ["agent_start","agent_end","agent_settled","session_before_compact","session_compact","session_shutdown"]) {
    pi.on(name, (_event, ctx) => {
      log({event:name,idle:ctx.isIdle()});
      if (name === "session_shutdown") clearInterval(timer);
    });
  }
  pi.on("message_end", (event) => log({event:"message_end",message:event.message}));
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
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            try:
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
        child = subprocess.Popen(command, cwd=out, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr, text=True, bufsize=1)
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

    def inject(batch, trigger):
        lines = ["Local news " + batch]
        digest = hashlib.sha256(json.dumps(lines, separators=(",", ":")).encode()).hexdigest()
        dispatch({"batchId": batch, "content": "\n".join(lines), "digest": digest, "triggerTurn": trigger})
        until(lambda: any(e.get("batchId") == batch for e in events()), process, "sendMessage return")

    def entries(path, batch):
        return [entry for entry in jsonl(path) if entry.get("type") == "custom_message" and entry.get("customType") == "hrk-news" and entry.get("details", {}).get("batchId") == batch]

    try:
        server = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
        thread = threading.Thread(target=server.serve_forever)
        thread.start()
        process, reader = start(argv)
        until(lambda: any(e["event"] == "session_start" for e in events()), process, "session start")
        identity = next(e for e in events() if e["event"] == "session_start")
        transcript = Path(identity["file"])
        result["identity"] = identity
        inject("fresh-no-turn", False)
        dispatch({"kind": "snapshot", "name": "fresh"})
        until(lambda: any(e.get("name") == "fresh" for e in events()), process, "fresh snapshot")
        snapshot = next(e for e in events() if e.get("name") == "fresh")
        result["checks"]["fresh_memory_entry"] = any(e.get("details", {}).get("batchId") == "fresh-no-turn" for e in snapshot["entries"])
        result["checks"]["fresh_disk_entry"] = bool(entries(transcript, "fresh-no-turn"))
        inject("fresh-trigger-turn", True)
        until(provider_started.is_set, process, "provider request")
        result["checks"]["before_assistant_disk_entry"] = bool(entries(transcript, "fresh-trigger-turn"))
        write_json(out / "before-assistant.json", {"fileExists": transcript.exists(), "entries": jsonl(transcript)})
        provider_release.set()
        until(lambda: any(e["event"] == "agent_end" for e in events()), process, "first turn end")
        until(lambda: bool(entries(transcript, "fresh-trigger-turn")), process, "transcript persistence")
        with transcript.open("rb") as handle:
            os.fsync(handle.fileno())
        directory = os.open(transcript.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        result["checks"]["file_and_directory_fsync"] = True
        shutil.copyfile(transcript, out / "persisted.jsonl")
        send({"id": "padding", "type": "prompt", "message": "padding " * 1500})
        until(lambda: sum(e["event"] == "agent_end" for e in events()) >= 2, process, "padding turn")
        send({"id": "compact", "type": "compact", "customInstructions": "Preserve news."})
        until(lambda: any(r.get("id") == "compact" for r in records), process, "compact")
        result["checks"]["compact_succeeded"] = any(r.get("id") == "compact" and r.get("success") for r in records)
        result["checks"]["compact_retained_entry"] = len(entries(transcript, "fresh-trigger-turn")) == 1
        stop(process, reader)
        resume_flag = "--session" if args.harness == "pi" else "--resume"
        process, reader = start(argv + [resume_flag, str(transcript)])
        until(lambda: sum(e["event"] == "session_start" for e in events()) == 2, process, "resume")
        resumed = [e for e in events() if e["event"] == "session_start"][-1]
        result["checks"]["resume_same_identity"] = (resumed["id"], resumed["file"]) == (identity["id"], identity["file"])
        inject("resumed", False)
        until(lambda: bool(entries(transcript, "resumed")), process, "resumed append")
        result["checks"]["resume_retained_entry"] = len(entries(transcript, "fresh-trigger-turn")) == 1
        result["checks"]["resumed_append"] = len(entries(transcript, "resumed")) == 1
        stop(process, reader)
        provider_started.clear()
        provider_release.clear()
        starts = sum(e["event"] == "session_start" for e in events())
        process, reader = start(argv)
        until(lambda: sum(e["event"] == "session_start" for e in events()) > starts, process, "fresh crash session")
        crash_identity = [e for e in events() if e["event"] == "session_start"][-1]
        crash_file = Path(crash_identity["file"])
        inject("crash-before-assistant", True)
        until(provider_started.is_set, process, "crash boundary provider request")
        result["checks"]["crash_target_exists_before_kill"] = crash_file.exists()
        process.kill()
        process.wait(timeout=3)
        stop(process, reader)
        result["checks"]["crash_target_exists_after_kill"] = crash_file.exists()
        write_json(out / "crash-target.json", crash_identity)
        provider_release.set()
        starts = sum(e["event"] == "session_start" for e in events())
        process, reader = start(argv + [resume_flag, str(crash_file)])
        try:
            until(lambda: sum(e["event"] == "session_start" for e in events()) > starts, process, "resume crashed target", timeout=10)
            crash_resumed = [e for e in events() if e["event"] == "session_start"][-1]
            write_json(out / "crash-resumed.json", crash_resumed)
            result["checks"]["crash_resume_same_identity"] = (crash_resumed["id"], crash_resumed["file"]) == (crash_identity["id"], crash_identity["file"])
        except (RuntimeError, TimeoutError) as error:
            result["checks"]["crash_resume_same_identity"] = False
            result["crashResumeError"] = str(error)
        stop(process, reader)
        bootstrap_file = out / "bootstrap.jsonl"
        bootstrap_file.touch(exist_ok=False)
        starts = sum(e["event"] == "session_start" for e in events())
        process, reader = start(argv + [resume_flag, str(bootstrap_file)])
        try:
            until(lambda: sum(e["event"] == "session_start" for e in events()) > starts, process, "bootstrap empty session", timeout=10)
            boot_identity = [e for e in events() if e["event"] == "session_start"][-1]
            boot_header = next((entry for entry in jsonl(bootstrap_file) if entry.get("type") == "session"), {})
            result["checks"]["bootstrap_header_persisted"] = boot_header.get("id") == boot_identity["id"] and boot_identity["file"] == str(bootstrap_file)
            write_json(out / "bootstrap-identity.json", boot_identity)
            if result["checks"]["bootstrap_header_persisted"]:
                provider_started.clear()
                provider_release.clear()
                inject("bootstrap-crash", True)
                until(provider_started.is_set, process, "bootstrap provider request")
                result["checks"]["bootstrap_custom_before_assistant"] = len(entries(bootstrap_file, "bootstrap-crash")) == 1
                process.kill()
                process.wait(timeout=3)
                stop(process, reader)
                provider_release.set()
                starts = sum(e["event"] == "session_start" for e in events())
                process, reader = start(argv + [resume_flag, str(bootstrap_file)])
                until(lambda: sum(e["event"] == "session_start" for e in events()) > starts, process, "bootstrap crash resume")
                boot_resumed = [e for e in events() if e["event"] == "session_start"][-1]
                result["checks"]["bootstrap_crash_resume_identity"] = (boot_identity["id"], boot_identity["file"]) == (boot_resumed["id"], boot_resumed["file"])
                result["checks"]["bootstrap_crash_retained_entry"] = len(entries(bootstrap_file, "bootstrap-crash")) == 1
        except (RuntimeError, TimeoutError, ValueError) as error:
            result["checks"]["bootstrap_header_persisted"] = False
            result["bootstrapError"] = str(error)
        if not result["checks"]["before_assistant_disk_entry"]:
            result["blockers"].append("A fresh custom-triggered turn reaches the provider before its transcript exists. A parent crash here leaves an immutable pending target with no file to verify or explicitly resume.")
        # These require a safe fresh-session persistence strategy before bridge work.
        result["checks"]["bridge_protocol"] = "not run: runtime durability gate first"
        result["checks"]["late_append_fencing"] = "not yet proven"
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
    finally:
        provider_release.set()
        if process is not None and reader is not None:
            stop(process, reader)
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
