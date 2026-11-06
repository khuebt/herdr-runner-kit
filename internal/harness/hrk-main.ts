// Main TUI only. Reload retains the original attempt, never permission to resend.
// @ts-nocheck
import { spawn } from "node:child_process";
import { isAbsolute } from "node:path";
import { env, pid } from "node:process";

const BIN = "__HRK_BIN__";
const HARNESS = "__HRK_HARNESS__";
const KEY = Symbol.for("herdr-runner-kit.main.v1");
const DEBOUNCE = 500;

export default function (api) {
  if (env.HRK_TASK || env.HRK_MAIN !== String(pid) || env.HRK_MAIN_HARNESS !== HARNESS ||
      env.HRK_MAIN_INTERACTIVE !== "1" || !env.HRK_MAIN_OWNER || !env.HRK_MAIN_NONCE) return;
  const identity = JSON.stringify([HARNESS, env.HRK_MAIN_OWNER, env.HRK_MAIN_NONCE]);
  let shared = globalThis[KEY];
  if (shared && shared.identity !== identity) return;
  if (!shared) shared = globalThis[KEY] = { identity, epoch: 0, pending: null, target: null, closing: Promise.resolve() };
  shared.dispose?.();
  const epoch = ++shared.epoch;
  let ctx, child, generation = 0, sequence = 0, registerID, receiptID;
  let timer, unsubscribe, tui, editor = shared.editor, closeReply, disposing, removeAbort;
  let stopped = false, transitioning = false, connected = false, fault = false, reconciled = false;
  let busy = false, submitting = false, retry = false, autoCompact = false, manualCompact = false, compactIsAuto = false, prompts = 0;
  let lastInput = Date.now(), lastNotice = 0, lastStatus, nextReceipt = 0, receiptDelay = 500;
  let nextConnect = 0, connectDelay = 1000, activityDue = false;
  const current = () => !stopped && shared.epoch === epoch;
  const sameTarget = (a, b) => a && b && a.sessionId === b.sessionId && a.sessionFile === b.sessionFile && a.cwd === b.cwd;
  const target = c => ({ sessionId: c.sessionManager.getSessionId(), sessionFile: c.sessionManager.getSessionFile(), cwd: c.sessionManager.getCwd() });
  const status = (message, warning = false) => {
    if (!current()) return;
    if (message !== lastStatus) { ctx?.ui?.setStatus?.("hrk-news", message); lastStatus = message; }
    if (warning && Date.now() - lastNotice > 30000) {
      lastNotice = Date.now(); ctx?.ui?.notify?.(message, "warning");
    }
  };
  const schedule = (delay = 0) => {
    if (!current() || transitioning) return;
    clearTimeout(timer); timer = setTimeout(tick, delay);
  };
  const request = (type, more = {}) => {
    if (!child || child.exitCode !== null || child.signalCode !== null || !child.stdin.writable) return;
    const requestId = `${epoch}-${++sequence}`;
    child.stdin.write(JSON.stringify({ version: 1, type, requestId, ...(generation ? { generation } : {}), ...more }) + "\n");
    return requestId;
  };
  const receipt = () => {
    if (!connected || !shared.pending || receiptID || Date.now() < nextReceipt) return;
    receiptID = request("receipt", { batchId: shared.pending.id });
    nextReceipt = Date.now() + receiptDelay; receiptDelay = Math.min(30000, receiptDelay * 2);
  };
  const receive = message => {
    if (message.version !== 1 || typeof message.requestId !== "string" || !["ready", "batch", "committed", "conflict", "error"].includes(message.type)) throw new Error("invalid news response");
    if (message.requestId === registerID && message.type === "ready") {
      if (!Number.isSafeInteger(message.generation) || message.generation < 1) throw new Error("invalid generation");
      generation = message.generation; connected = true; fault = false; connectDelay = 1000;
      if (shared.pending) receipt(); else status(undefined);
    } else if (message.generation && message.generation !== generation) return;
    if (message.requestId === receiptID) receiptID = undefined;
    if (message.type === "batch") {
      const b = message.batch;
      if (!connected || !b || b.batchId !== message.batchId || !sameTarget(b.target, shared.target) ||
          b.target.owner !== env.HRK_MAIN_OWNER || b.target.harness !== HARNESS ||
          typeof b.digest !== "string" || !Array.isArray(b.lines) || !b.lines.every(line => typeof line === "string")) throw new Error("invalid news batch");
      if (shared.pending && shared.pending.id !== b.batchId) throw new Error("overlapping news batches");
      if (!shared.pending) shared.pending = { id: b.batchId, batch: b, attempted: false };
      reconciled = true;
    } else if (message.type === "committed") {
      if (shared.pending?.id === message.batchId) shared.pending = null;
      receiptID = undefined; receiptDelay = 500; nextReceipt = 0; fault = false; status(undefined);
    } else if (message.type === "conflict" || message.type === "error") {
      if (message.batchId && !shared.pending) shared.pending = { id: message.batchId, attempted: true };
      // Only these replies establish absence on the immutable, verified target.
      const absent = shared.pending?.id === message.batchId &&
        (message.message === "prior append may still finish; verify its receipt or exit the main process and explicitly resume" ||
         message.message === "receipt not on disk; retain pending and do not resend");
      reconciled = absent; fault = !absent;
      status(absent ? "News pending: verifying the original transcript; exit and resume if it cannot finish." :
        "News paused: " + String(message.message || message.type).slice(0, 400), true);
      if (closeReply) { closeReply(false); closeReply = undefined; }
    } else if (message.type === "ready" && message.message === "closed") {
      connected = false;
      if (closeReply) { closeReply(true); closeReply = undefined; }
    }
    schedule();
  };
  const connect = () => {
    if (!current() || child || shared.retiring || !shared.target || transitioning) return;
    let buffer = "", forceStop;
    const launched = spawn(BIN, ["main", "__news"], { stdio: ["pipe", "pipe", "ignore"] });
    shared.released = false; child = launched; generation = 0; connected = false; reconciled = false; receiptID = undefined; nextReceipt = 0;
    const live = () => current() && child === launched;
    const failed = () => {
      if (!live()) return;
      fault = true; status("News child disconnected; pending news is preserved.", true); launched.kill("SIGTERM");
      if (!forceStop) forceStop = setTimeout(() => launched.kill("SIGKILL"), 700);
    };
    launched.on("error", failed); launched.stdin.on("error", failed);
    launched.stdout.setEncoding("utf8");
    launched.stdout.on("data", data => {
      if (!live()) return;
      buffer += data;
      try {
        if (Buffer.byteLength(buffer) > 4 * 1024 * 1024) throw new Error("oversized news response");
        let end;
        while ((end = buffer.indexOf("\n")) >= 0) {
          const line = buffer.slice(0, end); buffer = buffer.slice(end + 1); receive(JSON.parse(line));
        }
      } catch { failed(); }
    });
    launched.once("close", () => {
      clearTimeout(forceStop);
      if (!live()) return;
      child = undefined; connected = false; receiptID = undefined;
      if (closeReply) { closeReply(false); closeReply = undefined; }
      nextConnect = Date.now() + connectDelay; connectDelay = Math.min(connectDelay * 2, 30000);
      status("News child stopped; pending news is preserved.", true); schedule(connectDelay);
    });
    registerID = request("register", { harness: HARNESS, ...shared.target, parentPID: pid, nonce: env.HRK_MAIN_NONCE, quiesced: true });
  };
  const observeUI = host => {
    tui = host;
    const found = [];
    const walk = (node, depth = 0) => {
      if (!node || depth > 12 || found.length > 1) return;
      if (typeof node.getText === "function" && typeof node.onSubmit === "function") found.push(node);
      if (Array.isArray(node.children)) for (const item of node.children) walk(item, depth + 1);
    };
    walk(host);
    const focus = host.getFocusedComponent?.() ?? host.getFocused?.();
    if (found.length === 1) editor = found[0];
    else if (focus?.constructor?.name === "CustomEditor") editor = focus;
    shared.editor = editor;
    return { render: () => [], invalidate() {} };
  };
  const safe = () => {
    if (!ctx || ctx.mode !== "tui" || ctx.hasUI !== true || !ctx.model || !tui || !editor ||
        typeof tui.hasOverlay !== "function") return false;
    return !busy && !submitting && !retry && !autoCompact && !manualCompact && prompts === 0 &&
      ctx.isIdle() === true && ctx.hasPendingMessages() === false &&
      !tui.hasOverlay() && (tui.getFocusedComponent?.() ?? tui.getFocused?.()) === editor &&
      ctx.ui.getEditorText().length === 0 && Date.now() - lastInput >= DEBOUNCE;
  };
  function tick() {
    timer = undefined;
    if (!current() || transitioning) return;
    try {
      if (!sameTarget(target(ctx), shared.target)) { fault = true; status("News paused: resume the original session before changing its target.", true); return; }
      if (!child && Date.now() >= nextConnect) connect();
      if (!connected) { schedule(Math.max(500, nextConnect - Date.now())); return; }
      if (activityDue) { request("activity"); activityDue = false; }
      const pending = shared.pending;
      if (!pending) return;
      if (pending.attempted) {
        receipt(); if (!fault) status("News pending: waiting for a durable receipt. Exit and resume if the old append cannot finish.");
        schedule(Math.max(500, nextReceipt - Date.now())); return;
      }
      if (fault || !reconciled) { receipt(); schedule(Math.max(500, nextReceipt - Date.now())); return; }
      if (!safe()) {
        status("News pending: finish setup or dismiss a dialog, then leave the editor empty."); schedule(500); return;
      }
      pending.attempted = true;
      const b = pending.batch;
      api.sendMessage({ customType: "hrk-news", content: b.lines.join("\n"), display: true,
        details: { batchId: b.batchId, owner: env.HRK_MAIN_OWNER, digest: b.digest } },
      { deliverAs: "followUp", triggerTurn: true });
      receipt(); schedule(500);
    } catch { status("News paused: runtime state is unavailable; pending news is preserved.", true); schedule(1000); }
  }
  const stopChild = async () => {
    const old = child;
    if (!old) return false;
    let closed = false, deadline;
    if (connected && !shared.pending) {
      closed = await new Promise(resolve => {
        closeReply = resolve; deadline = setTimeout(() => resolve(false), 600); request("close", { quiesced: true });
      });
      clearTimeout(deadline); closeReply = undefined;
    }
    child = undefined; connected = false; receiptID = undefined;
    shared.retiring = old;
    old.once("close", () => { if (shared.retiring === old) shared.retiring = undefined; });
    if (old.exitCode !== null || old.signalCode !== null) { shared.retiring = undefined; return closed; }
    await new Promise(resolve => {
      const kill = setTimeout(() => old.kill("SIGKILL"), 700);
      const limit = setTimeout(finish, 1100);
      function finish() { clearTimeout(kill); clearTimeout(limit); resolve(); }
      old.once("close", finish); old.stdin.end(); old.kill("SIGTERM");
    });
    return closed;
  };
  const dispose = () => {
    if (disposing) return disposing;
    transitioning = true; clearTimeout(timer); timer = undefined;
    unsubscribe?.(); unsubscribe = undefined; removeAbort?.();
    ctx?.ui?.setWidget?.("hrk-news-observer", undefined); ctx?.ui?.setStatus?.("hrk-news", undefined);
    disposing = shared.closing = stopChild().then(closed => { if (closed) shared.released = true; }).finally(() => { stopped = true; });
    return disposing;
  };
  shared.dispose = dispose;
  const beforeTransition = async (_event, tree = false) => {
    if (!current()) return;
    if (shared.pending || !connected || transitioning) {
      nextReceipt = 0; receipt(); status("Finish pending news before changing session or branch; exit and resume recovers uncertain delivery.", true);
      return { cancel: true };
    }
    transitioning = true; clearTimeout(timer);
    const closed = await stopChild();
    if (!closed || shared.pending) {
      transitioning = false; schedule(); status("News arrived during the transition; reconcile it before changing session.", true);
      return { cancel: true };
    }
    shared.released = true;
    status("News paused for session change. If it is cancelled or fails, use /reload to reconnect.");
    if (tree) {
      // The runtime includes branch summarization in isIdle, even on abort/failure.
      let wasBusy = ctx.isIdle() === false;
      const settledTree = () => {
        if (!current() || !transitioning) return;
        try {
          if (ctx.isIdle() === false) wasBusy = true;
          else if (wasBusy && sameTarget(target(ctx), shared.target)) {
            transitioning = false; schedule(); return;
          }
        } catch { status("News paused until session navigation finishes.", true); }
        timer = setTimeout(settledTree, 500);
      };
      timer = setTimeout(settledTree, 0);
    }
  };
  const on = (name, handler) => api.on(name, (event, context) => {
    if (!current() && name !== "session_start") return;
    return handler(event || {}, context);
  });
  on("session_start", (_event, context) => {
    if (shared.epoch !== epoch) return;
    const next = target(context);
    if (!next.sessionId || !isAbsolute(next.sessionFile || "") || !isAbsolute(next.cwd || "")) return;
    ctx = context;
    if (ctx.mode !== "tui" || ctx.hasUI !== true) return;
    if (shared.target && !sameTarget(shared.target, next) && (!shared.released || shared.pending)) { status("News paused: explicitly resume the original session.", true); return; }
    if (child) return;
    stopped = false; disposing = undefined; transitioning = false; shared.target = next;
    unsubscribe?.();
    unsubscribe = ctx.ui.onTerminalInput(() => {
      if (!current()) return;
      lastInput = Date.now(); activityDue = true; schedule(DEBOUNCE);
      // Return nothing: the runtime owns every key and every paste byte.
    });
    ctx.ui.setWidget("hrk-news-observer", observeUI);
    shared.closing.then(() => { if (current()) { connect(); schedule(DEBOUNCE); } });
  });
  on("input", (event, context) => {
    if (event.source === "extension") return;
    // Omp reloads plugins without recreating extensions; also recover a cancelled switch.
    const command = event.text?.trim();
    if ((command === "/reload" || (HARNESS === "omp" && command === "/reload-plugins")) && transitioning && shared.released && !shared.pending && sameTarget(target(context), shared.target)) transitioning = false;
    submitting = typeof event.text === "string" && event.text.length > 0 && !event.text.startsWith("/");
    lastInput = Date.now(); activityDue = true; schedule(DEBOUNCE);
  });
  on("agent_start", () => { busy = true; submitting = false; });
  const settled = () => { busy = false; activityDue = true; nextReceipt = 0; schedule(); };
  on("agent_settled", () => { if (HARNESS === "pi") settled(); });
  on("agent_end", event => { if (HARNESS === "omp" && event.willContinue !== true) settled(); });
  on("session_stop", () => { if (HARNESS === "omp") schedule(); });
  on("message_end", () => { nextReceipt = 0; schedule(); });
  on("auto_retry_start", () => { retry = true; });
  on("auto_retry_end", () => { retry = false; schedule(); });
  on("ui_prompt_start", () => { prompts++; });
  on("ui_prompt_end", () => { prompts = Math.max(0, prompts - 1); schedule(); });
  on("auto_compaction_start", () => { autoCompact = true; });
  on("auto_compaction_end", () => { autoCompact = false; if (compactIsAuto) endCompact(); schedule(); });
  const endCompact = () => { manualCompact = false; compactIsAuto = false; removeAbort?.(); removeAbort = undefined; schedule(); };
  on("session_before_compact", event => {
    removeAbort?.(); manualCompact = true; compactIsAuto = autoCompact || event.reason === "threshold" || event.reason === "overflow";
    const aborted = () => { if (current()) endCompact(); };
    event.signal?.addEventListener("abort", aborted, { once: true });
    removeAbort = () => event.signal?.removeEventListener("abort", aborted);
    if (event.signal?.aborted) aborted();
  });
  for (const name of ["session_compact", "session_compact_failed"]) on(name, endCompact);
  for (const name of ["session_before_switch", "session_before_fork"]) on(name, event => beforeTransition(event));
  on("session_before_tree", event => beforeTransition(event, true));
  on("session_tree", (_event, context) => { transitioning = false; shared.target = target(context); connect(); schedule(); });
  on("session_shutdown", dispose);
}
