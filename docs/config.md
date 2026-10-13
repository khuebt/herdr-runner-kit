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
| `telegram.notify` | every accepted value | Events that send a Telegram message. `done` is sent only for a task that no other task waits on. Accepted values: `needs-human`, `failed`, `stalled`, `held`, `done`, `turn-ended-unreported`. |
| `quotaFallback` | none | Agents a runner switches to, in order, when its agent hits a usage limit. Each entry has an `agent` spec and, for a Claude agent only, an optional `claudeConfig` folder, as a path or a name such as `claude2`; without it the runner keeps its own. An entry that is the current agent, already hit its limit on the task, or names a missing folder is skipped. With none left the task fails. See [commands.md](commands.md#usage-limits). |
| `updateCheck` | `true` | Lets `doctor` look for a newer release, at most once a day. `version --check` always looks. See [upgrading.md](upgrading.md). |

The token and chat ids are read when a message is sent. They are never copied into the kit's config or state.

## Default kinds

| Kind | Agent | Worktree |
|---|---|---|
| `code` | `codex` | yes |
| `flow` | `claude:claude-opus-5-5:high` | yes |
| `fix` | `claude:claude-sonnet-5-5:medium` | no |
| `research` | `claude:claude-opus-5-5:high` | no |
| `report` | `claude:claude-opus-5-5:high` | no |

An explicit `--agent` or `--worktree` / `--no-worktree` on `spawn` always wins over the table.

## Agent spec

`harness[:model[:effort]]`

- `harness` is `claude` or `codex`.
- `model` is passed to the CLI as is (`--model` for Claude, `-m` for Codex). Leave it empty to use the CLI's own default.
- `effort` is optional. Claude accepts `low`, `medium`, `high`, `xhigh` and `max`. Codex accepts `minimal`, `low`, `medium`, `high` and `xhigh`.

Examples: `codex`, `codex:gpt-6-astra:xhigh`, `claude::max`, `claude:claude-sonnet-5-5`.

## What runners are allowed to do

Runners start with permission prompts turned off, so they never stall waiting for approval:

- Claude Code runs with `--dangerously-skip-permissions`.
- Codex runs with `--dangerously-bypass-approvals-and-sandbox`, which also turns off its sandbox. A Codex runner can read and write anything your user account can.

Before the first Codex runner in a repository, the kit adds that repository root to `~/.codex/config.toml` as trusted, because Codex asks again in every new worktree otherwise. Running `spawn` in a repository is taken as your consent for that repository. The kit appends the entry and never changes existing lines. If you marked the repository as untrusted in Codex, `spawn` stops and tells you instead.

For Claude, the same consent applies: the kit marks the folder the runner starts in (the repository, or the runner's worktree) as trusted in the runner's Claude config: `<config folder>/.claude.json`, or `~/.claude.json` for the default `~/.claude`. Claude asks again for a git repository even when a parent folder is trusted, so copying parent trust is not enough. Approval of external `CLAUDE.md` imports is only copied from the repository's own entry; the kit never approves imports you did not approve. When the kit writes nothing, because Claude has never run with that config folder or the folder is already trusted, the event log gets a `claude-trust-skipped` event for the task saying which. `doctor` checks that Claude has run with each config folder runners use; see [setup.md](setup.md#doctor).

If a trust dialog still appears, the task moves to `needs-human` and waits up to 30 minutes for you to answer it in the runner's tab.

## State

Task state lives in `${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit/`, or in `$HRK_STATE_DIR` when that is set. The directory is created with mode `0700` and every file with `0600`, because it holds prompts:

- `state.json` records the schema version;
- `seq` is the task id counter;
- `tasks/<id>/task.json` holds one task, next to its runner files (`contract.md`, `claude-settings.json`, `launch.log`) and its `out/` directory;
- `events.jsonl` is the event log shared by every task.

Do not edit these files by hand. When the schema changes, a new binary migrates the directory forward on first use. An older binary refuses state written by a newer one.
