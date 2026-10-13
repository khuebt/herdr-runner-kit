# herdr-runner-kit commands

Every command prints short plain text. `herdr-runner-kit <command> --help` lists every flag.

## Tasks

| Command | Use |
|---|---|
| `spawn [flags] --stdin` | Record a task and open its runner. Returns at once. |
| `list [--all]` | Your tasks with stored status and herdr's live state. `--all` shows every owner. |
| `show <id>` | One task in full: prompt, summary, question, outputs, branch. |
| `send <id> --stdin` | Type a new prompt into a live runner. |
| `answer <id> --stdin` | Answer a `needs-human` question. |
| `answer --from-telegram --stdin` | Route a Telegram reply by its `#id` tag. |
| `release <id> [--note TEXT]` | Start a `held` task. |
| `focus <id>` | Switch herdr to the runner's tab. |
| `stop <id> [--remove-worktree]` | Quit the runner and close its tab. Branches are always kept. |
| `gc` | Close runners of tasks that finished 30 minutes ago or more. |
| `events [--since 1h] [--task ID] [--timings]` | The event log, or measured latencies. |

## spawn flags

| Flag | Meaning |
|---|---|
| `--kind NAME` | `code`, `flow`, `fix`, `research`, `report`, or a kind from the user's config. |
| `--agent SPEC` | `claude` or `codex`, with optional `:model` and `:effort`. |
| `--worktree`, `--no-worktree` | Override the kind's worktree choice. |
| `--base REF` | Git ref a new worktree branch starts from. |
| `--skill NAME` | Skill to invoke first, such as `ak:cook`. |
| `--after t1,t2` | Start when every listed task is `done`. |
| `--gate auto\|human` | `human` holds the task for `release`. |
| `--title TEXT` | Short title; by default it comes from the prompt. |
| `--claude-config DIR` | Claude config folder (account) of a Claude runner: a path, or a name such as `claude2` for `~/.claude2`. Default: the main session's. |
| `--cwd DIR` | Folder the task works in; by default the current folder. |

## Task statuses

`queued` (waiting for parents) → `held` (human gate) → `starting` → `running` → `done`, `needs-human`, `failed` or `stopped`. A `needs-human` task returns to `running` after `answer`.

## News that wait reports

| Line starts with | Meaning | What you do |
|---|---|---|
| `tN done` | Finished; summary and outputs follow | Relay it. |
| `tN needs-human` | The runner asks a question | Ask the user, then `answer`. |
| `tN held` | A `--gate human` task waits | Ask the user, then `release`. |
| `tN failed` | The runner or its launch failed | Relay the reason; offer to retry with a new `spawn`. |
| `tN stalled` | The agent waits on a prompt in its pane | Tell the user to look at it (`focus`). |
| `tN turn-ended-unreported` | The runner stopped without reporting | `send` it a nudge to report, or tell the user. |
| `tN stopped` | Stopped by `stop` | Relay it. |
