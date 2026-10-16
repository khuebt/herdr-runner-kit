# Architecture

This page is for contributors. It describes how the kit's Go packages are layered, which package may import which, and how to add a harness, a backend, a notifier, a workflow or a command without touching the core.

The core (task store, runner lifecycle, wait, CLI) should not change when an integration is added. The boundary tests in [`internal/archtest`](../internal/archtest/arch_test.go) enforce the tables below; change the test and this page together.

## Layers

| Layer | Packages | May import | Rule |
|---|---|---|---|
| Data | `task`, `state`, `config`, `buildinfo`, `fsutil`, `redact`, `skill` | each other only | No integration package. `task` keeps the harness names and one data table (effort levels, whether a model is required). `skill` embeds the skill files `setup` installs. |
| Core lifecycle | `runner`, `wait` | data, each other, `harness` (the port), `herdr`, `telegram`, `planrun`, `project` | Never names a harness. `herdr` and `telegram` are allowed as concrete clients and are the next seams. |
| Harness port and adapters | `harness` | `task`, `fsutil` | One file per harness (`claude.go`, `codex.go`, and `pi.go`, which registers pi and omp as two variants of one implementation) plus `harness.go`: the `Adapter` interface, the registry and shared helpers. Never imports the core. |
| Integrations | `herdr`, `telegram`, `install`, `upgrade` | data only | Concrete clients. An interface is introduced when a second implementation arrives, not before. |
| Workflows | `project`, `planrun` | data, `harness`, `herdr` | Built on the runner's task model; they never reach into runner internals. |
| Composition root | `cli`, `cmd/hrk` | everything | Wires concrete clients into the core; the only place that may know every package. |

All packages live under `internal/`, except `cmd/hrk` and `skill` at the repository root.

## Dependency rules

1. A package imports only what its layer allows. A new internal package needs a row in the layer table first.
2. Outside `internal/harness` and `internal/task`, no code names a harness constant other than `task.Claude` (so no `task.Codex`, `task.Pi` or `task.Omp`), and `task.Claude` appears only in the allowlisted Claude-only features below. The core lifecycle packages also never spell a harness name as a string, such as `"codex"`. Test files are exempt.
3. `internal/harness` imports no core package (`runner`, `wait`, `state`, `config`, `cli`).

### Claude-only features

The Claude config folder and the main session are features of one harness, not per-harness switches, so they keep their `task.Claude` checks. Each allowlist entry in the boundary test carries its reason; a new entry needs one too.

| Where | Why |
|---|---|
| `runner/fallback.go` `agentKey` | A Claude config folder is part of a Claude agent's identity in `quotaFallback`. |
| `runner/spawn.go` `newTask` | `--claude-config` applies to Claude runners only. |
| `config/config.go` `Validate` | `quotaFallback[].claudeConfig` applies to Claude agents only. |
| `cli/doctor.go` `runnerClaudeConfigs` | Doctor checks the Claude config folders that Claude tasks recorded. |
| `project/templates.go` `RenderOutputs` | Output paths render no skill, so any harness gives the same paths. |

The main session (`hrk main`) always runs Claude Code and names the `claude` command directly.

## The harness seam

`harness.Adapter` holds every per-harness decision. The core asks the adapter and acts on the answer:

| Decision | Adapter method |
|---|---|
| Launch argv, herdr kind, skill syntax, first prompt | `LaunchArgs`, `HerdrKind`, `RenderSkill`, `FirstPrompt` |
| Per-task runner files (Claude: `claude-settings.json`, `contract.md`; pi and omp: `contract.md`, the runner extension `hrk-runner.ts`, and for omp `omp-runner.yml`) | `Files`; the core writes them into the task folder with mode 0600 |
| Folder trust before launch | `PrepareTrust`, which returns the event to record |
| Startup dialogs, screen reading, retries, usage limit on screen, context footer | `Dialogs`, `ProgressText`, `ShowsRetry`, `ShowsUsageLimit`, `ContextLeft`, `SelfResolvingPrompts` |
| Reading a turn end | `ReadTurnEnd` returns a `TurnSignal`: ignore, thread, confirmed limit, limit on screen, background work, context left |
| How the hook gets its input and answers | `HookPayload`, `HookReply`, `NudgeInHook`, `SessionContext` |
| Background work before a sync | `WorksInBackground` |
| Whether a done waits for the turn end, and the agent's working status line | `DoneAtTurnEnd`, `ShowsWorking` |
| Whether the runner's input can be locked, how to clear it, and the prompt gate's reading and reply | `LocksInput`, `ClearInputKeys`, `GatePrompt`, `GateBlock` |
| What doctor checks | `Checks`: binary, flags, herdr integration, required or not |

`TurnEnd` keeps one order of checks for every harness: a confirmed limit, an ignored turn, the thread, a held done or a queued message, a limit on screen, low context, background work, and only then the nudge.

A done becomes final when the runner reports it, unless the adapter's `DoneAtTurnEnd` is true (Codex). Then a done the runner reports from its session is held on the task as `pendingDone`, the task stays `running`, and only a `done-pending` progress event is written. The turn end makes it final through the normal report path: a fresh git snapshot, the `done` news and the dependents. A `send` while the agent works (herdr says `working`, or `ShowsWorking` sees the status line), or while the task holds a done, records `queuedSend` before it types the prompt, and takes it back if the prompt fails. Codex adds such a message to the running turn, so at the turn end a done reported after the send counts, while one reported before it is superseded (`done-superseded`) and the turn is nudged as unreported; the runner reports again on the message. If no turn end comes, `wait`'s `WatchPendingDone` makes a done older than 2 minutes final once herdr sees the agent idle on two reads 5 s apart with no status line on screen. A held done that no message superseded also becomes final when the agent or its pane goes away, or when a usage limit would hand the task to a fallback: the work is finished, so the task neither fails nor runs again. A manual report, and every report of Claude, pi and omp, is final at once.

Every prompt the kit types goes through one helper, `typePrompt` (internal/runner/prompt_type.go): the first prompt, `send`, `answer`, stall nudges and resumes, handoff requests and typed turn-end nudges. When the task was spawned with `runnerInput: locked` and the adapter's `LocksInput` is true (Claude, pi and omp), it sends `ClearInputKeys`, adds a one-time token to `tasks/<id>/prompt-tokens` (an expiry 30 s ahead, one per line, under `prompt-tokens.lock`), and then types the prompt; a prompt herdr fails to type takes its token back. An expired token stays on file until the gate lets a submit through: while any token is unused, the kit's last prompt never reached the gate (its Enter was lost), so the next prompt skips the clear keys and goes in joined to it. The agent's prompt-submit hook, `hrk __prompt-gate`, lets a submit through when `GatePrompt` says the agent wrote it, or when it takes a token, and otherwise prints `GateBlock` and writes an `input-blocked` event. A token, not the prompt's text, because Claude rewrites pastes. The gate fails open on its own errors. For pi and omp the caller is the runner extension's `input` handler (and pi's `user_bash`), which runs the gate with a 2 s timeout and drops the prompt with a warning when the gate prints a reason; anything else, including a failed gate, lets it through.

## Extension recipes

### Add a harness

1. Add a row to the harness table in `internal/task/agent_spec.go`: name, effort levels, whether a model is required.
2. Add `internal/harness/<name>.go` implementing `Adapter`, and register it in `adapters` in `harness.go`, in the same order as the task table.
3. Add screen captures and hook payloads under `internal/harness/testdata` and tests against them.
4. Document the agent spec in [configuration](config.md) and [commands](commands.md).

Nothing in `runner`, `wait`, `cli` or `config` changes. The registry test fails until the task table and the adapters name the same harnesses in the same order.

### Add a backend or a notifier

herdr (the terminal backend) and Telegram (the notifier) are concrete clients with one implementation each. When a second one is needed, such as Orca for the backend or another chat for the notifier:

1. Define the interface in the core package that uses it, holding only the methods the core calls today.
2. Make the existing client satisfy it unchanged, then add the second implementation as its own integration package.
3. Choose the implementation in `cli`, the composition root, and update the layer table and the boundary test together.

### Add a workflow

A workflow such as `project` or `planrun` builds on the task model: it creates tasks through the runner's public API and reads their state. It may import data packages, `harness` and `herdr`, never runner internals.

### Add a command

Commands live in `internal/cli`, one file per command or group, registered in `root.go`. A command wires concrete clients into the core and prints the result; behaviour belongs in the core or a workflow, where it can be tested without the CLI.

## Testing conventions

- Agent screens and hook payloads are captured from live runs and kept under `internal/harness/testdata`. Tests read them as they are; a new agent version gets new captures rather than edited ones.
- Runner tests use the fake herdr in `internal/runner/runner_test.go`, which records every call and answers from per-command handlers.
- Live checks against real agents use a scratch build (`go build -o <scratch>/hrk ./cmd/hrk`) with `HRK_STATE_DIR` and `XDG_CONFIG_HOME` pointed at scratch folders, so the user's task store and `config.yaml` are never read or written.

## Developing against a running install

The installed kit may be running the user's main session and runners while you work. Keep it untouched:

- Work on a branch in its own worktree; leave the main checkout on `main`.
- Never install a branch build: no `go install`, no `install.sh`, no copy into `~/.local/bin`, and no `hrk setup` or `hrk upgrade` run from it.
- Never point a branch build at the real state folder or config; use scratch `HRK_STATE_DIR` and `XDG_CONFIG_HOME`.
- A config that names something the installed version does not know stops every `hrk` command, so add it to the real config only after upgrading.
