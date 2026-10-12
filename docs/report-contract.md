# Report contract

Every runner gets a short contract that tells it how to report back to the main session. The contract is versioned by `contractVersion` (see [versioning.md](versioning.md)). This page describes version 1.

## How the runner gets it

- **Claude Code** gets the contract as an appended system prompt (`--append-system-prompt-file`), so it survives context compaction. The file is `tasks/<id>/contract.md` in the state directory.
- **Codex** has no appended system prompt, so the contract is added to the end of its first prompt.

The first prompt is the rendered skill (if `--skill` was given) followed by your prompt. Skills render as `/ak:plan` for Claude and `$ak-plan` for Codex, where a colon becomes a dash.

## What the runner is told

The runner reports exactly once, with one of:

```sh
herdr-runner-kit report done --summary "<one or two sentences>" [--output <path>]...
herdr-runner-kit report needs-human --question "<one question, with the recommended option>" [--summary "<context>"]
herdr-runner-kit report failed --summary "<what failed and why>"
```

The command path is the absolute path of the binary that spawned the runner.

Rules the runner follows:

- `HRK_TASK=<id>` is set in its environment, so `report` knows which task is reporting.
- Files meant for the main session go under `tasks/<id>/out/` and are passed with `--output`.
- `needs-human` is only for a decision the runner cannot make. It asks one question, lists the options, recommends one, and stops. The answer arrives in the same session with `send`; the runner continues and reports again.
- Background shells and subagents finish before `done` is reported.
- Commits use Conventional Commits, with no AI attribution and no plan or task ids. Files under `plans/` and agent folders (`.claude/`, `.codex/`, `.agents/`) are never committed. The runner never pushes and never opens a pull request.

## Compatibility

Within one major product version the contract only grows. A runner started by an older binary can still report after an upgrade.
