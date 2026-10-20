# Report contract

Every runner gets a short contract that tells it how to report back to the main session. The contract is versioned by `contractVersion` (see [versioning.md](versioning.md)). This page describes version 3.

## How the runner gets it

- **Claude Code** gets the contract as an appended system prompt (`--append-system-prompt-file`), so it survives context compaction. The file is `tasks/<id>/contract.md` in the state directory.
- **Codex** has no appended system prompt, so the contract is added to the end of its first prompt.

The first prompt is the rendered skill (if `--skill` was given) followed by your prompt. Skills render as `/ak:plan` for Claude and `$ak-plan` for Codex, where a colon becomes a dash.

## What the runner is told

The runner reports once when it finishes, with one of the commands below. A later message from the main session (`send`) is new work: `send` to a `done` or `failed` task whose tab is still open sets it `running` again; a `stopped` task is never reopened, and the runner reports again when that work is done.

```sh
hrk report done --summary "<one or two sentences>" [--decision "<question> -> <choice>"]... [--output <path>]...
hrk report needs-human --question "<question, with the options and the recommended one>"... [--summary "<context>"]
hrk report failed --summary "<what failed and why>"
hrk report handoff --output <handoff file> --summary "<what is done and what is left>"
```

`report handoff` is only for a runner the kit told that its context is nearly full; see [Context handoff](#context-handoff).

The command path is the absolute path of the binary that spawned the runner. `report` also records a local git snapshot of the task's checkout, capped at 2 seconds and never fetching, which `wait` shows as `git:` on the news line. For a runner in its own worktree the snapshot also lists, as `stacked:`, up to 10 local branches its branch builds on: merged into it but not yet into the default branch.

### Question rounds

`needs-human` takes one `--question`, or a round of up to 5 (each at most 400 characters). A single question longer than that is cut with `…`, so a version 1 report is never refused; a round with a longer question is. A round is stored as one question, numbered one per line, so `answer`, Telegram replies and `status` treat it like any other question. `wait` prints it as a header and one indented line per question:

```text
t4 needs-human · pick a store · 2 questions
  1. Which database? a) Postgres (recommended) b) SQLite
  2. Which host? a) VPS (recommended) b) PaaS
```

Telegram shows the same round with `Reply: #t4 1<option> 2<option>, such as 1a 2b`. Answer it the usual way, for example `hrk answer t4 "1a 2b"`.

### Decisions

`done` takes up to 7 `--decision "<question> -> <choice>"` (each at most 200 characters): open questions the runner settled itself with the recommended option. `wait` shows each on its own indented line below the news line, as `decided: d`, and Telegram as `Decided:` lines. `--decision` with `needs-human` or `failed` is refused.

### Answers typed in the tab

When a runner reports while its question is still open, the user answered it in the runner's tab. The report then records an `answered` event with the summary `answered in the runner's tab` before its own event.

Rules the runner follows:

- `HRK_TASK=<id>` is set in its environment, so `report` knows which task is reporting.
- Files meant for the main session go under `tasks/<id>/out/` and are passed with `--output`.
- `needs-human` is only for decisions the runner cannot make. It asks one question or one round of up to 5, lists the options for each, recommends one, and stops. The answer arrives in the same session with `send`; the runner continues and reports again.
- Every open question it settles itself with the recommended option is passed to `done` as `--decision`.
- It never asks for, types or echoes a secret value in a question, summary or file; it asks where the secret is stored instead. `report` hides anything that looks like a secret (token-like strings, values of `password=`-style keys, credentials in URLs) in its summary, questions and decisions before it stores them or sends them to Telegram, and the `answered` event of `answer` and the `sent` event of `send` do the same.
- Background shells and subagents finish before `done` is reported. A Claude `done` is held until a turn end with no background subagent running (see below).
- Shared services (databases, containers, dev servers) it did not create for the task stay running; it stops only what it created, and names any shared service it started in its report.
- A read-only lookup goes to a fresh read-only subagent on a cheaper model, such as Claude Code's `Explore` agent with `model: sonnet`, briefed with paths; the runner never forks its whole context for it. The rule is added to contract version 3 as is.
- A `done` summary names each check the runner ran and its result, such as `go test ./... passed`, and any check it could not run and why. It never claims a check it did not run. The rule is added to contract version 3 as is.
- Only when the kit says its context is nearly full, it commits or saves its work at a stable point, writes a handoff under `tasks/<id>/out/` naming what is done, what is left, the checks it ran and any background process it started (command and pid), then runs `report handoff` and stops.
- It does not run `rm -r` on directories, alone or inside compound commands, because Claude Code stops on a safety prompt for it; scratch output goes into a new folder under `tasks/<id>/out/` instead.
- Commits use Conventional Commits, with no AI attribution and no plan or task ids. Files under `plans/` and agent folders (`.claude/`, `.codex/`, `.agents/`) are never committed. The runner never pushes and never opens a pull request.
- It commits on the branch it starts on, and never creates, switches or deletes branches. If its prompt names another branch, it stays on its own and says so in its summary.
- With a report language set (`language` in [config.md](config.md), or `spawn --language`), it writes summaries and questions in that language.
- With `heavySlots` above 0 ([config.md](config.md)), it runs tests, lint, typecheck and builds as `hrk heavy -- <command>`, which waits for a free slot shared by every runner and the main session. The rule is added to contract version 3 as is; a runner that does not follow it still works. See [commands.md](commands.md#heavy-commands).

### Notes on a report

`report` never refuses a report for its length or its output paths, so every version 2 runner keeps working. It prints a note on stderr, which the runner sees in its tool output, when:

- the summary is longer than 400 characters: `note: summaries over 400 characters are cut in wait and Telegram; keep one or two sentences and put detail in an --output file under <out dir>`;
- an `--output` lies outside the task's out folder: `note: <path> is outside <out dir>; files for the main session belong there`.

The notes are neither events nor stored.

## Context handoff

A long runner degrades once its agent compacts its context. The kit lets each task continue once in a fresh session instead:

1. **The kit asks.** For Claude Code, the trigger is the first automatic compaction of the session: the kit's `PreCompact` hook sees `trigger: auto`, and the `SessionStart` hook that follows with `source: compact` adds the request to the model's context and queues it in the runner's tab as a message. A manual `/compact` never asks, and neither does an automatic compaction that Claude announced and then replaced with a manual one. For Codex, a turn that ends with its footer showing `contextHandoffAt` percent of the context left or less (15 by default) gets the request instead of the usual nudge, and does not count as an unreported turn. The footer reads `N% context left`, or `Context N% left` or `Context N% used` from a status line with `context-remaining` or `context-used`; a footer cut by the pane width, or without the item, never asks. Each attempt is asked once, and a runner that was asked and ends its turn without handing off gets the request again as its nudge.
2. **The runner hands off.** The request names the file, `tasks/<id>/out/handoff-a<N>.md` with `N` the next attempt, and the exact `report handoff` command. `report handoff` needs `--summary` and exactly one `--output`, an existing file, and is accepted only while the task is `running`.
3. **The kit starts the next attempt.** The task's `attempt` goes up by one and the file is added to its `handoffs`. A detached launcher quits the agent with Ctrl+C, the way a quota switch does, and starts the same agent in the same tab and checkout. The new session's first prompt is the task's prompt followed by `This is attempt N of task tN. The previous session neared its context limit and wrote a handoff: <file>. Read it first, check git status and git log, continue only the unfinished work, rerun only the checks it affects, then report.` Processes the previous session left running are listed after it (`Still running from the previous session: …`) and in a `processes-carried` event; the kit never stops them.

Each task hands off at most once. A second `report handoff` is refused with `tN already used its one context replacement; keep working in this session (it compacts on its own), or report failed or needs-human`, and later automatic compactions are only recorded.

The event log gets `context-low` when the kit asks, or when an automatic compaction asks nothing (with the reason, such as `contextHandoff is off`), and `handoff` when the next attempt starts. `contextHandoff: false` turns the requests off (see [config.md](config.md#keys)).

### Attempts

From the second attempt on, the launcher sets `HRK_ATTEMPT=<n>` in the runner's environment. `report` and the turn-end and context hooks compare it with the task's attempt, where an empty value means 1:

- a report from an ended attempt is refused: `tN is on attempt 2; a report from attempt 1 is ignored`;
- its turn ends and compaction hooks are ignored;
- a manual `report --task` names no attempt and is never refused for it.

News of a later attempt names it: `wait` adds `attempt 2` to a `needs-human` line, Telegram tags it `#t3a2`, and `answer t3a2` or a `#t3a2` reply reaches it. A reply that names another attempt is refused, such as `#t3a1` once attempt 2 runs: `that question was for attempt 1 of t3, which ended`. News of the first attempt carries no attempt, so a late reply to it, tagged `#t3`, reaches the current attempt like any bare id.

A quota switch keeps the attempt but starts a fresh session, so a handoff request the previous session got does not carry over. Its first prompt says the previous agent hit its usage limit and, from the second attempt on, points at the last handoff too.

## A runner that forgets to report

Each runner's turn-end hook (Claude's Stop hook, Codex's `notify`) checks whether the task reported. Claude's `StopFailure` hook, for a turn that ended in an API error, looks for a usage limit (see [commands.md](commands.md#usage-limits)) or an error that may pass (see [commands.md](commands.md#api-errors)):

- The first turn that ends without a report is nudged: Claude's Stop hook keeps it working with a reminder, and Codex gets the reminder as a new prompt. The reminder reads: "You ended your turn without reporting to the main session. If background shells or agents are still running, end your turn now without reporting: you are woken when they finish. Never start sleep or heartbeat commands to stay awake. Otherwise report: …", followed by the three `report` commands.
- The second becomes `turn-ended-unreported` news for the main session.
- Later turns are ignored until the main session sends or answers again.
- A Claude turn that ends on a transient API error, such as a lost connection, is not counted: the kit types its own continue prompt once instead of a nudge, and a second failure in a row is `api-error` news.

A Claude turn that ends while background shells or agents still run is not counted, because Claude wakes on its own when they finish. The kit reads them from the Stop hook's `background_tasks` list (Claude Code 2.1.300 and later): any entry still `running` or `pending` means the turn is not over. A Claude `done` stays held while the input lists running background work that ends, such as a `subagent` or `workflow` (any type but `shell` and `monitor`); a background shell, such as a dev server, never holds it. A held done is never handed off, and becomes final after 30 minutes without a turn end (`done-hold-timeout` event). Claude's mode line is read only when the hook input has no such list, because it counts shells but not subagents, and draws its count a second or more after they start. Codex's internal title-generation turn is ignored. If your Codex `config.toml` sets its own `notify`, the kit still calls it with the same argument.

## Compatibility

Within one major product version the contract only grows. A runner started by an older binary can still report after an upgrade: a version 3 binary accepts every version 1 and 2 report, and such a report writes the same task and event as before. Version 3 adds `report handoff` and `HRK_ATTEMPT`; the task fields `attempt` and `handoffs`, and the event field `attempt`, appear only after a handoff. Version 2 added `--decision` and question rounds, with the task fields `decisions` and `git.stacked` and the event field `decisions`.

Going back is not safe while runners of a newer contract are open: a version 2 binary rejects `report handoff`, and drops a task's `attempt` when it rewrites the task, so the attempt checks stop; a version 1 binary keeps only the last `--question` of a round and rejects `--decision` as an unknown flag.
