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
telegram:
  notify: [needs-human, failed]
```

## Keys

| Key | Default | Meaning |
|---|---|---|
| `schemaVersion` | `1` | Format of this file. A file written for a newer version is refused with "upgrade herdr-runner-kit". |
| `defaultKind` | `code` | Kind used when `spawn` gets no `--kind`. It must be defined under `kinds`. |
| `kinds.<name>.agent` | see below | Agent spec for tasks of this kind. Required for a new kind. |
| `kinds.<name>.worktree` | see below | `true` runs the task in a new git worktree when its cwd is inside a git repository. |
| `worktreeBranchPrefix` | `hrk/` | Prefix of runner branches, for example `hrk/t3-fix-login`. |
| `telegram.stateDir` | the plugin's directory | Directory holding the Telegram channel plugin's `.env` and `access.json`. By default it is `$TELEGRAM_STATE_DIR`, else `${CLAUDE_CONFIG_DIR:-~/.claude}/channels/telegram`. |
| `telegram.chatId` | the plugin's `allowFrom` | Chat to notify, when it should differ from the plugin's allow list. |
| `telegram.notify` | `[needs-human, failed, stalled, held, done]` | Events that send a Telegram message. `done` is sent only for a task that no other task waits on. Accepted values: `needs-human`, `failed`, `stalled`, `held`, `done`. |

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

For Claude, the kit copies the trust you already gave the repository, or one of its parent folders, to the runner's worktree path in `.claude.json`. It never grants trust you did not give.

## State

Task state lives in `${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit/`. The directory is created with mode `0700` and every file with `0600`, because it holds prompts:

- `state.json` records the schema version;
- `seq` is the task id counter;
- `tasks/<id>/task.json` holds one task;
- `events.jsonl` is the event log shared by every task.

Do not edit these files by hand. When the schema changes, a new binary migrates the directory forward on first use. An older binary refuses state written by a newer one.
