# Configuration

herdr-runner-kit works with no configuration file. To change a default, create:

```
${XDG_CONFIG_HOME:-~/.config}/herdr-runner-kit/config.yaml
```

Every key is optional. The file is merged over the built-in defaults, key by key. An unknown key is an error that names the key, so a typo never passes silently.

## Example

```yaml
schemaVersion: 1
defaultKind: code
kinds:
  code:
    agent: codex:gpt-6-astra:high
  fix:
    worktree: true
  review:                 # a new kind
    agent: claude:claude-opus-5-5:high
    worktree: false
worktreeBranchPrefix: hrk/
language: Vietnamese
telegram:
  notify: [needs-human, failed]
quotaFallback:            # tried in order when a runner hits a usage limit
  - agent: claude:claude-opus-5-5:high
    claudeConfig: claude2
  - agent: codex
  - agent: omp:openai-codex/gpt-6-luna:high
keepFor: 2h               # kept tabs close 2 hours after the task finished
stallAfter: 15m           # nudge an agent that shows no progress for 15 minutes
contextHandoffAt: 20      # ask a Codex, pi or omp runner to hand off at 20% context left
runnerMemoryMax: 16G      # cap each runner at 16 GB (Linux with systemd)
heavySlots: 1             # run one wrapped test, lint or build command at a time
worktreeSetup:            # run in each new runner worktree of this repository
  ~/Work/easify-cs-dashboard:
    - cp "$HRK_REPO_ROOT/.env" .env
    - npm ci
runnerInput: open         # let text typed in a runner's tab reach its agent
```

## Keys

| Key | Default | Meaning |
|---|---|---|
| `schemaVersion` | `1` | Format of this file. A file written for a newer version is refused with "upgrade herdr-runner-kit". |
| `defaultKind` | `code` | Kind used when `spawn` gets no `--kind`. It must be defined under `kinds`. |
| `kinds.<name>.agent` | see below | Agent spec for tasks of this kind. Required for a new kind. |
| `kinds.<name>.worktree` | see below | `true` runs the task in a new git worktree when its cwd is inside a git repository. |
| `worktreeBranchPrefix` | `hrk/` | Prefix of runner branches, for example `hrk/t3-fix-login`. |
| `language` | none | Language runners write report summaries and questions in, as free text such as `Vietnamese`, on one line of at most 40 characters. `spawn --language` overrides it per task. See [report-contract.md](report-contract.md). |
| `telegram.envFile` | none | Dotenv file holding `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID`, used instead of the plugin's files. `~/` is expanded. See [telegram.md](telegram.md). |
| `telegram.stateDir` | the plugin's directory | Directory holding the Telegram channel plugin's `.env` and `access.json`. By default it is `$TELEGRAM_STATE_DIR`, else `${CLAUDE_CONFIG_DIR:-~/.claude}/channels/telegram`. |
| `telegram.chatId` | the plugin's `allowFrom` | Chat to notify, when it should differ from the plugin's allow list. |
| `telegram.notify` | every accepted value | Events that send a Telegram message. A task's first `done` is held back while a task that waits on it has not started; see [telegram.md](telegram.md#messages). Accepted values: `needs-human`, `failed`, `stalled`, `held`, `done`, `turn-ended-unreported`, `usage-limit`. |
| `quotaFallback` | none | Agents a runner switches to, in order, when its agent hits a usage limit. Each entry has an `agent` spec, such as `omp:openai-codex/gpt-6-luna:high`, and, for a Claude agent only, an optional `claudeConfig` folder, as a path or a name such as `claude2`; without it the runner keeps its own. An entry that is the current agent, already hit its limit on the task, or names a missing folder is skipped. With none left, a limit Claude, pi or omp reports fails the task, and a limit only seen on a Codex screen, or a pi or omp error that only may be one, becomes `usage-limit` news instead. See [commands.md](commands.md#usage-limits). |
| `keepFor` | `4h` | How long a kept runner's tab stays open after its task finished, as a duration such as `4h` or `90m`. `gc`, and `wait` while it runs, then close it once its agent is idle. `0` keeps it until `keep --off` or `stop`, as before this key existed. See [commands.md](commands.md#closing-runners). |
| `stallAfter` | `10m` | How long a `working` agent may show no visible progress before the kit nudges it, then interrupts it, then reports it `stalled` with the runner left open. `0` turns the check off; otherwise at least `2m`. See [commands.md](commands.md#what-wait-reports). |
| `contextHandoff` | `true` | Lets the kit ask a runner near its context limit to hand off once to a fresh session of its task: a Claude runner at its first automatic compaction, a Codex runner at a turn end with little context left, and a pi or omp runner at either. `false` leaves runners to compact on their own. See [commands.md](commands.md#context-handoff). |
| `contextHandoffAt` | `15` | For Codex, pi and omp runners: the context left, in percent of the window, at or below which a turn end asks for the handoff. Codex's footer shows it; for pi and omp the kit's runner extension sends it at each turn end. From 5 to 50. |
| `runnerMemoryMax` | `25%` | Memory cap of each runner on Linux with systemd: a size such as `16G` or `8192M` (`K`, `M`, `G` or `T`), a share of RAM from `1%` to `100%`, or `off`. See [Runner memory cap](#runner-memory-cap). |
| `heavySlots` | `2` | How many commands wrapped in `hrk heavy` run at once across every runner and the main session; the others wait for a free slot. From 0 to 64; `0` runs them at once and drops the rule from the runner contract. See [commands.md](commands.md#heavy-commands). |
| `worktreeSetup` | none | Shell commands, per repository, that run in each new runner worktree before its agent starts. Each key is a repository's main checkout, as an absolute or `~/` path; each value a list of at least one command. See [Worktree setup](#worktree-setup). |
| `runnerInput` | `locked` | `locked` lets a Claude, pi or omp runner take prompts only from the kit: `spawn`, `send`, `answer`, Telegram replies and the kit's own nudges. `open` lets text typed in the runner's tab reach its agent, as before this key existed. A change applies to runners spawned after it. Codex runners stay open. See [Runner input](#runner-input). |
| `updateCheck` | `true` | Lets `doctor` look for a newer release, at most once a day. `version --check` always looks. See [upgrading.md](upgrading.md). |

The token and chat ids are read when a message is sent. They are never copied into the kit's config or state.

## Default kinds

| Kind | Agent | Worktree |
|---|---|---|
| `code` | `claude:claude-sonnet-5-5:medium` | yes |
| `flow` | `claude:claude-opus-5-5:high` | yes |
| `fix` | `claude:claude-sonnet-5-5:medium` | no |
| `research` | `claude:claude-opus-5-5:high` | no |
| `report` | `claude:claude-opus-5-5:high` | no |

An explicit `--agent` or `--worktree` / `--no-worktree` on `spawn` always wins over the table.

## Agent spec

`harness[:model[:effort]]`

- `harness` is `claude`, `codex`, `pi` or `omp` (oh-my-pi).
- `model` is passed to the CLI as is (`--model` for Claude, pi and omp, `-m` for Codex). Leave it empty to use the CLI's own default. pi and omp require it, as `provider/model`, because without one omp picks whichever provider it finds first; a bare `pi` or `omp` is refused by `spawn` and by this file.
- `effort` is optional. Claude accepts `low`, `medium`, `high`, `xhigh` and `max`. Codex accepts `minimal`, `low`, `medium`, `high` and `xhigh`. pi accepts `off`, `minimal`, `low`, `medium`, `high`, `xhigh` and `max`, and omp the same plus `auto`; both get it as `--thinking`.

Examples: `codex`, `codex:gpt-6-astra:xhigh`, `claude::max`, `claude:claude-sonnet-5-5`, `pi:openai-codex/gpt-6-luna:high`, `omp:openai-codex/gpt-6-luna:max`.

pi's own `--model provider/id:thinking` shorthand does not apply: the spec's third field replaces it, so write `pi:openai-codex/gpt-6-luna:high`. A pi or omp model id may contain colons, as in `pi:ollama/qwen3:8b` or `omp:openrouter/vendor/model:free:high`: the last part is read as the effort only when it is an effort name, and an effort name the harness does not accept is an error.

## Worktree setup

A new worktree holds only the repository's tracked files: no `node_modules`, no `.env`, nothing else git ignores. A runner's lint, typecheck and tests then fail with errors such as `eslint: not found`. `worktreeSetup` fills the worktree before the runner's agent starts:

```yaml
worktreeSetup:
  ~/Work/easify-cs-dashboard:
    - cp "$HRK_REPO_ROOT/.env" .env
    - npm ci
  /srv/api:
    - ln -s "$HRK_REPO_ROOT/node_modules" node_modules
```

- **When.** Once per task, right after the kit creates the task's own worktree. A quota fallback, a context handoff, and a `plan run` phase that works in the run's first checkout get no setup, since their worktree already had it. A task without a worktree never gets one.
- **How.** Each command runs in turn with `sh -c` in the worktree, with the runner's environment plus `HRK_REPO_ROOT` (the main checkout) and `HRK_WORKTREE` (the new worktree). Their output goes to `worktree-setup.log` in the task's out folder. The step holds one `heavySlots` slot while it runs, so parallel `npm ci` runs queue like tests, and it stops at the first command that fails. The whole step, the wait for a slot included, has 15 minutes; then its commands are killed.
- **Result.** On success the `started` event says `setup: 2 steps, 41s`. On a failure the runner still starts: the event log gets a `worktree-setup-failed` event with the command, its exit code and the log path, and the runner's first prompt says the setup failed and asks it to fix that before running checks, or to report `needs-human`.
- **Trust.** The commands come only from this file, never from the repository, so a cloned repository cannot make the kit run code. They run as given, as your own shell would run them.
- **Cleanup.** Copied files that git ignores, such as `.env` and `node_modules`, do not keep `cleanup` from removing a merged worktree. Untracked files git does not ignore do, so list such a file in `.gitignore` before copying it in.

## Runner input

A runner's tab shows its agent at work, and nothing stops a stray keystroke there from reaching the model as a prompt. With `runnerInput: locked`, the default, a Claude, pi or omp runner takes prompts only from the kit, so you steer it from the main session, with `hrk send` and `hrk answer`, or from Telegram.

- **How, in Claude.** The runner's Claude settings add a `UserPromptSubmit` hook, `hrk __prompt-gate`. Each time the kit types a prompt it first clears the input box with `ctrl+u` and `backspace`, which do not interrupt a running turn, then writes a one-time token, valid for 30 seconds, into the task folder (`prompt-tokens`). The gate lets a submit through when it takes a token, or when the prompt is Claude's own `<task-notification>` that a background shell or subagent ended. Any other prompt is refused, and the tab shows `<id> is a runner: it takes prompts only from the main session. Ask there, or run: hrk send <id> "…"`. Claude keeps the refused text in the input; the kit clears it before its next prompt, unless its own last prompt never went in, as when its Enter was lost: then the next prompt goes in joined to it.
- **How, in pi and omp.** The runner extension, `hrk-runner.ts`, asks the same gate about each prompt submitted in the tab, with the same token, since both agents give the kit's typed prompt and a human's the same source. A refused prompt is dropped, the input is cleared, and the reason shows as a warning. `!` bash mode is refused too: omp sends it through the same check, and pi's `user_bash` event shows the reason as the command's output, with exit 1, instead of running it. A gate that takes over 2 seconds lets the prompt through.
- **What a locked Claude tab still allows.** Watching and scrolling; answering dialogs, such as a trust or permission prompt, with keys; `esc` to interrupt the agent; built-in slash commands such as `/clear`, `/model` or `/exit`; and `!` bash mode, which no hook can stop. Skill slash commands are prompts, and are refused.
- **Events.** Each refused prompt adds an `input-blocked` event, which is not news, with its first 60 characters and anything that looks like a secret hidden. A runner that reports while its question is open moved on by itself, so a locked task gets no `answered in the runner's tab` event and `events --learning` counts no answer for it.
- **Failing open.** When the gate cannot read its input or the state, it lets the prompt through and writes the reason to stderr: the lock prevents accidents, and must never cut the kit off from its own runner.
- **What a locked pi or omp tab still allows.** Watching and scrolling; `esc` to interrupt; and built-in slash commands such as pi's `/session`, which the extension never sees.
- **Codex.** Codex marks a per-run prompt hook `New hook - review required` and skips it until you trust it, so Codex runners stay open; their `started` event says `input open (Codex cannot lock it yet)`.

## Runner memory cap

herdr runs every pane, and so every runner and the main session, inside its own service's memory group. Without a cap, one runner running several test suites at once can fill the machine's memory, and systemd-oomd then kills herdr with every session in it.

On Linux, once a runner's agent has started and before its first prompt, the kit moves the agent into a new systemd scope capped at `runnerMemoryMax`. It asks the user's systemd manager over D-Bus with `busctl --user`, the same call `systemd-run --scope` makes:

```
busctl --user call org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager \
  StartTransientUnit 'ssa(sv)a(sa(sv))' hrk-<task>-<attempt>-<unix seconds>.scope fail 6 \
  PIDs au 1 <agent pid> MemoryMax t <bytes> MemorySwapMax t 0 CollectMode s inactive-or-failed \
  Delegate b true OOMPolicy s continue 0
```

The scope starts with the agent alone, so a short-lived child that exits on the way cannot fail it. The processes the agent already started, such as its MCP servers, then join it one by one with `AttachProcessesToUnit`, which needs `Delegate`; the kit lists them again and moves the new ones, up to 3 rounds, until none is left outside. A process that exited meanwhile is skipped. Any still outside after that are counted in the `started` event, as `memory scope hrk-t3-1-1760000000.scope (2 of its processes stayed outside)`.

Nothing is typed into the pane and the pane's shell stays where it is: only the agent and every command it starts from then on are in the scope. A share such as `25%` is taken of the machine's memory. Memory the agent used before the move stays counted against herdr's group; what it and its commands use from then on counts against the cap.

- A command that goes over the cap is killed inside its runner only; `MemorySwapMax=0` keeps it from swapping instead. The kernel picks the largest process in the scope, usually the test worker, not the agent. Where systemd handles OOM kills in scopes, as Debian 12's 252.5 and later and Ubuntu 24.04's 255 do, a scope would stop, killing the agent too, so the kit asks for `OOMPolicy=continue`. When systemd rejects that property, the kit asks again without it and the `started` event says `systemd rejected OOMPolicy=continue, so an OOM kill may end the runner`: Ubuntu 22.04's 249 keeps such a scope running, but systemd 252.0 to 252.4, and other builds without the backport, end the whole scope, agent included.
- The cap is per runner, not a total: four runners at `25%` can still fill memory together. Each runner is then its own group, so systemd-oomd kills the runner using the memory instead of herdr.
- Each agent session, including a quota fallback or a context handoff, gets a new scope; processes the previous session left keep the old one, and systemd drops a scope once it is empty. The `started` event names it, as `memory scope hrk-t3-1-1760000000.scope`, and `systemctl --user status <scope>` shows its processes and limit.
- When the scope cannot be made, the agent keeps running where herdr started it, and the `started` event says why, as `no memory scope: <reason>`. A launch never fails because of it.
- The scopes are not part of herdr's service, so a herdr restart, or systemd-oomd killing herdr, no longer ends the commands a runner started. The kit never kills them: it reports the processes still running in the runner's scopes as `processes-left` news when it fails a runner as lost, when `stop` or `gc` closes one, even when herdr no longer has the runner's pane, and once when `wait` sees that herdr restarted a runner's pane, whose agent herdr then resumes outside the scope. See [commands.md](commands.md#closing-runners).
- When herdr restores a pane after its own restart, herdr starts the agent again itself, outside any scope. That runner runs without a cap until its next agent session.

On macOS, and with `runnerMemoryMax: off`, agents start exactly as before and share herdr's memory. `doctor` moves a short-lived process into a scope the same way to show the cap on its `runner memory cap` line, or why there is none.

## What runners are allowed to do

Runners start with permission prompts turned off, so they never stall waiting for approval:

- Claude Code runs with `--dangerously-skip-permissions`.
- Codex runs with `--dangerously-bypass-approvals-and-sandbox`, which also turns off its sandbox. A Codex runner can read and write anything your user account can.
- pi runs with `--approve`, which trusts the project's own settings, extensions and skills for this run only and saves nothing to `~/.pi`. A pi runner, like a Codex one, can do anything your user account can.
- omp runs with `--auto-approve`, plus a per-task config overlay (`omp-runner.yml`) that turns off its `ask` tool, which would wait for an answer forever, and the run-state spinner in its terminal title (`tui.titleState: false`). herdr counts each title change as a new agent revision, so with the spinner on, a hung runner would look busy and stall detection could never flag it. Approval policies you set under `tools.approval` in your own omp config still apply and may still prompt; the runner then shows as blocked.

Before the first Codex runner in a repository, the kit adds that repository root to `~/.codex/config.toml` as trusted, because Codex asks again in every new worktree otherwise. Running `spawn` in a repository is taken as your consent for that repository. The kit appends the entry and never changes existing lines. If you marked the repository as untrusted in Codex, `spawn` stops and tells you instead.

For Claude, the same consent applies: the kit marks the folder the runner starts in (the repository, or the runner's worktree) as trusted in the runner's Claude config: `<config folder>/.claude.json`, or `~/.claude.json` for the default `~/.claude`. Claude asks again for a git repository even when a parent folder is trusted, so copying parent trust is not enough. Approval of external `CLAUDE.md` imports is only copied from the repository's own entry; the kit never approves imports you did not approve. When the kit writes nothing, because Claude has never run with that config folder or the folder is already trusted, the event log gets a `claude-trust-skipped` event for the task saying which. `doctor` checks that Claude has run with each config folder runners use; see [setup.md](setup.md#doctor).

pi and omp need no trust file: the kit writes nothing under `~/.pi` or `~/.omp`. If pi's "Trust project folder?" dialog shows anyway, the task moves to `needs-human` like any trust dialog.

If a trust dialog still appears, the task moves to `needs-human` and waits up to 30 minutes for you to answer it in the runner's tab.

## State

Task state lives in `${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit/`, or in `$HRK_STATE_DIR` when that is set. The directory is created with mode `0700` and every file with `0600`, because it holds prompts:

- `state.json` records the schema version;
- `seq` is the task id counter;
- `tasks/<id>/task.json` holds one task, next to its runner files (`contract.md`, `claude-settings.json` for Claude, `hrk-runner.ts` for pi and omp, `omp-runner.yml` for omp, `launch.log`) and its `out/` directory;
- `events.jsonl` is the event log shared by every task.

Do not edit these files by hand. When the schema changes, a new binary migrates the directory forward on first use. An older binary refuses state written by a newer one.
