# Report contract

Every runner gets a short contract that tells it how to report back to the main session. The contract is versioned by `contractVersion` (see [versioning.md](versioning.md)). This page describes version 2.

## How the runner gets it

- **Claude Code** gets the contract as an appended system prompt (`--append-system-prompt-file`), so it survives context compaction. The file is `tasks/<id>/contract.md` in the state directory.
- **Codex** has no appended system prompt, so the contract is added to the end of its first prompt.

The first prompt is the rendered skill (if `--skill` was given) followed by your prompt. Skills render as `/ak:plan` for Claude and `$ak-plan` for Codex, where a colon becomes a dash.

## What the runner is told

The runner reports once when it finishes, with one of the commands below. A later message from the main session (`send`) is new work: `send` to a `done` or `failed` task whose tab is still open sets it `running` again; a `stopped` task is never reopened, and the runner reports again when that work is done.

```sh
herdr-runner-kit report done --summary "<one or two sentences>" [--decision "<question> -> <choice>"]... [--output <path>]...
herdr-runner-kit report needs-human --question "<question, with the options and the recommended one>"... [--summary "<context>"]
herdr-runner-kit report failed --summary "<what failed and why>"
```

The command path is the absolute path of the binary that spawned the runner. `report` also records a local git snapshot of the task's checkout, capped at 2 seconds and never fetching, which `wait` shows as `git:` on the news line. For a runner in its own worktree the snapshot also lists, as `stacked:`, up to 10 local branches its branch builds on: merged into it but not yet into the default branch.

### Question rounds

`needs-human` takes one `--question`, or a round of up to 5 (each at most 400 characters). A single question longer than that is cut with `…`, so a version 1 report is never refused; a round with a longer question is. A round is stored as one question, numbered one per line, so `answer`, Telegram replies and `status` treat it like any other question. `wait` prints it as a header and one indented line per question:

```text
t4 needs-human · pick a store · 2 questions
  1. Which database? a) Postgres (recommended) b) SQLite
  2. Which host? a) VPS (recommended) b) PaaS
```

Telegram shows the same round with `Reply: #t4 1<option> 2<option>, such as 1a 2b`. Answer it the usual way, for example `herdr-runner-kit answer t4 "1a 2b"`.

### Decisions

`done` takes up to 7 `--decision "<question> -> <choice>"` (each at most 200 characters): open questions the runner settled itself with the recommended option. `wait` shows them first, as `decided: d1; d2` right after the title, and Telegram as `Decided:` lines. `--decision` with `needs-human` or `failed` is refused.

### Answers typed in the tab

When a runner reports while its question is still open, the user answered it in the runner's tab. The report then records an `answered` event with the summary `answered in the runner's tab` before its own event.

Rules the runner follows:

- `HRK_TASK=<id>` is set in its environment, so `report` knows which task is reporting.
- Files meant for the main session go under `tasks/<id>/out/` and are passed with `--output`.
- `needs-human` is only for decisions the runner cannot make. It asks one question or one round of up to 5, lists the options for each, recommends one, and stops. The answer arrives in the same session with `send`; the runner continues and reports again.
- Every open question it settles itself with the recommended option is passed to `done` as `--decision`.
- It never asks for, types or echoes a secret value in a question, summary or file; it asks where the secret is stored instead. `report` hides anything that looks like a secret (token-like strings, values of `password=`-style keys, credentials in URLs) in its summary, questions and decisions before it stores them or sends them to Telegram, and the `answered` event of `answer` and the `sent` event of `send` do the same.
- Background shells and subagents finish before `done` is reported.
- It does not run `rm -r` on directories, alone or inside compound commands, because Claude Code stops on a safety prompt for it; scratch output goes into a new folder under `tasks/<id>/out/` instead.
- Commits use Conventional Commits, with no AI attribution and no plan or task ids. Files under `plans/` and agent folders (`.claude/`, `.codex/`, `.agents/`) are never committed. The runner never pushes and never opens a pull request.
- It commits on the branch it starts on, and never creates, switches or deletes branches. If its prompt names another branch, it stays on its own and says so in its summary.
- With a report language set (`language` in [config.md](config.md), or `spawn --language`), it writes summaries and questions in that language.

### Notes on a report

`report` never refuses a report for its length or its output paths, so every version 2 runner keeps working. It prints a note on stderr, which the runner sees in its tool output, when:

- the summary is longer than 400 characters: `note: summaries over 400 characters are cut in wait and Telegram; keep one or two sentences and put detail in an --output file under <out dir>`;
- an `--output` lies outside the task's out folder: `note: <path> is outside <out dir>; files for the main session belong there`.

The notes are neither events nor stored.

## A runner that forgets to report

Each runner's turn-end hook (Claude's Stop hook, Codex's `notify`) checks whether the task reported. Claude's `StopFailure` hook, for a turn that ended in an API error, only looks for a usage limit (see [commands.md](commands.md#usage-limits)):

- The first turn that ends without a report is nudged: Claude's Stop hook keeps it working with a reminder, and Codex gets the reminder as a new prompt. The reminder reads: "You ended your turn without reporting to the main session. If background shells or agents are still running, end your turn now without reporting: you are woken when they finish. Never start sleep or heartbeat commands to stay awake. Otherwise report: …", followed by the three `report` commands.
- The second becomes `turn-ended-unreported` news for the main session.
- Later turns are ignored until the main session sends or answers again.

A Claude turn that ends while background shells or agents still run is not counted, because Claude wakes on its own when they finish. The kit reads them from the Stop hook's `background_tasks` list (Claude Code 2.1.300 and later): any entry still `running` or `pending` means the turn is not over. Claude's mode line is read only when the hook input has no such list, because it counts shells but not subagents, and draws its count a second or more after they start. Codex's internal title-generation turn is ignored. If your Codex `config.toml` sets its own `notify`, the kit still calls it with the same argument.

## Compatibility

Within one major product version the contract only grows. A runner started by an older binary can still report after an upgrade: a version 2 binary accepts every version 1 report, and a version 1 report (one `--question`, or `done` without `--decision`) writes the same task and event as before. The new task fields `decisions` and `git.stacked`, and the event field `decisions`, appear only when set. The `rm` rule and the report notes change no command or flag, so the contract stays at version 2.

Going back is not safe while version 2 runners are open: a version 1 binary keeps only the last `--question` of a round and rejects `--decision` as an unknown flag.
