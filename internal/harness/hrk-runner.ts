// hrk-runner.ts is the herdr-runner-kit runner extension. The kit writes it
// into each pi or omp task folder with the kit binary and the harness filled
// in, and the agent loads it with `-e` (pi) or `--hook` (omp). It tells the
// kit when a run ends and when the session compacts; it never changes what
// the agent does, and no failure in it reaches the agent.
//
// Events follow a live probe of pi 1.2.0 and omp 18.2.10:
//   - pi ends a run with agent_end, agent_before_settle{outcome} and
//     agent_settled{aborted}, once per prompt; Esc skips agent_before_settle.
//   - omp has no agent_settled. A run ends with session_stop then agent_end,
//     where willContinue === true means its async jobs keep the run alive;
//     omp can send duplicate or late ends.
//   - pi compacts with session_before_compact{reason} then session_compact;
//     omp wraps an automatic one in auto_compaction_start/_end.
//
// The source is plain JavaScript, so both agents load it as is.
// @ts-nocheck
import { spawn } from "node:child_process";

const BIN = "__HRK_BIN__";
const HARNESS = "__HRK_HARNESS__";

// State of the current run.
let run = 0; // counts runs, so a late timer never speaks for a newer one
let sent = false; // the run's turn end was sent
let outcome; // pi's agent_before_settle outcome
let stopReason; // the last assistant message's stop reason
let errorMessage; // and its error message
let autoCompact = false; // omp is inside an automatic compaction

// send runs `<bin> <hook> --harness <h>` with payload on stdin, detached and
// unawaited. The child inherits the tab's HRK_* environment.
function send(hook, payload) {
  try {
    const child = spawn(BIN, [hook, "--harness", HARNESS], {
      detached: true,
      stdio: ["pipe", "ignore", "ignore"],
    });
    child.on("error", () => {});
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload));
    child.unref();
  } catch {}
}

// usage reads the context gauge, leaving out what the agent does not know,
// as percent right after a compaction.
function usage(ctx) {
  try {
    const u = ctx?.getContextUsage?.();
    const c = {};
    if (typeof u?.percent === "number") c.percent = u.percent;
    if (typeof u?.tokens === "number") c.tokens = u.tokens;
    if (typeof u?.contextWindow === "number") c.window = u.contextWindow;
    return Object.keys(c).length > 0 ? c : undefined;
  } catch {
    return undefined;
  }
}

// readLast keeps the stop reason and error of the run's last assistant
// message.
function readLast(messages) {
  if (!Array.isArray(messages)) return;
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m?.role === "assistant") {
      stopReason = typeof m.stopReason === "string" ? m.stopReason : undefined;
      errorMessage = typeof m.errorMessage === "string" ? m.errorMessage : undefined;
      return;
    }
  }
}

// turnEnd sends the run's turn end, once per run.
function turnEnd(event, aborted, ctx) {
  if (sent) return;
  sent = true;
  const payload = { event, aborted: aborted === true || stopReason === "aborted" };
  if (outcome !== undefined) payload.outcome = outcome;
  if (stopReason !== undefined) payload.stopReason = stopReason;
  if (errorMessage !== undefined) payload.errorMessage = errorMessage;
  const context = usage(ctx);
  if (context !== undefined) payload.context = context;
  send("__turn-end", payload);
}

export default function (pi) {
  const on = (name, handler) => {
    try {
      pi.on(name, (event, ctx) => {
        try {
          handler(event ?? {}, ctx);
        } catch {}
      });
    } catch {}
  };

  on("agent_start", () => {
    run++;
    sent = false;
    outcome = stopReason = errorMessage = undefined;
  });
  on("agent_end", (event, ctx) => {
    readLast(event.messages);
    if (HARNESS === "omp" && event.willContinue !== true) turnEnd("agent_end", event.aborted, ctx);
  });
  on("agent_before_settle", (event) => {
    if (typeof event.outcome === "string") outcome = event.outcome;
  });
  on("agent_settled", (event, ctx) => {
    if (HARNESS === "pi") turnEnd("agent_settled", event.aborted, ctx);
  });
  // omp sends session_stop just before agent_end; the wait lets agent_end
  // fill in the last message, and sends the turn end if it never comes.
  on("session_stop", (event, ctx) => {
    if (HARNESS !== "omp") return;
    const stopped = run;
    setTimeout(() => {
      if (run === stopped) turnEnd("session_stop", event.aborted, ctx);
    }, 200);
  });

  // Compactions go to `__context` in Claude Code's hook shape.
  on("auto_compaction_start", () => {
    autoCompact = true;
  });
  on("auto_compaction_end", () => {
    autoCompact = false;
  });
  on("session_before_compact", (event) => {
    const auto = autoCompact || event.reason === "threshold" || event.reason === "overflow";
    send("__context", { hook_event_name: "PreCompact", trigger: auto ? "auto" : "manual" });
  });
  on("session_compact", () => {
    send("__context", { hook_event_name: "SessionStart", source: "compact" });
  });
}
