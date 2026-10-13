# herdr-runner-kit commands

Every command prints short plain text. `herdr-runner-kit <command> --help` lists every flag.

## Tasks

| Command | Use |
|---|---|
| `spawn [flags] --stdin` | Record a task and open its runner. Returns at once. |
| `list [--all]` | Your tasks with stored status, herdr's live state, and TAB: `open` (with when `gc` closes it), `kept`, `closed` or `-`. `--all` shows every owner. |
| `show <id>` | One task in full: prompt, summary, question, outputs, branch. |
| `send <id> --stdin` | Type a new prompt into a live runner. A done or failed task runs again; a stopped one refuses. |
| `answer <id> --stdin` | Answer a `needs-human` question. |
| `answer --from-telegram --stdin` | Route a Telegram reply by its `#id` tag. |
| `release <id> [--note TEXT]` | Start a `held` task. |
| `focus <id>` | Switch herdr to the runner's tab. |
| `stop <id> [--remove-worktree]` | Quit the runner and close its tab. Branches are always kept. |
| `gc` | Close runners of tasks that finished 30 minutes ago or more, except kept ones. |
| `keep <id> [--off]` | Keep a finished runner's tab open instead of letting `gc` close it. `--off` lets `gc` close it again. |
| `cleanup [id]... [--yes]` | List finished worktrees and branches left behind: merged, unmerged or dirty. `--yes` removes the merged ones. |
| `events [--since 1h] [--task ID] [--timings]` | The event log, or measured latencies. |
| `events --learning [--task ID]` | Per task: agents, attempts (starts plus reopening sends), nudges, sends, answers, agent switches, duration and time waited on a human after the spawn. |
| `sync <id> [--from REF] [--no-send]` | Merge origin's default branch (such as `origin/main`), or REF, into a worktree task's branch and ask the runner to re-run its checks. Refuses a busy agent, background work, an agent herdr cannot check, or a dirty worktree, and fetches only after those checks; a conflict is aborted and the text to `send` is printed. When it suggests `--from main`, local main has commits origin lacks. |

## spawn flags

| Flag | Meaning |
|---|---|
| `--kind NAME` | `code`, `flow`, `fix`, `research`, `report`, or a kind from the user's config. |
| `--agent SPEC` | `claude` or `codex`, with optional `:model` and `:effort`. |
| `--worktree`, `--no-worktree` | Override the kind's worktree choice. |
| `--branch NAME` | Name of the new worktree branch, following the repository's convention. |
| `--base REF` | Git ref a new worktree branch starts from. |
| `--skill NAME` | Skill to invoke first, such as `ak:cook`. |
| `--after t1,t2` | Start when every listed task is `done`. |
| `--gate auto\|human` | `human` holds the task for `release`. |
| `--title TEXT` | Short title; by default it comes from the prompt. |
| `--language NAME` | Language of the runner's report summaries and questions, such as `Vietnamese`. Default: the config's `language`. |
| `--claude-config DIR` | Claude config folder (account) of a Claude runner: a path, or a name such as `claude2` for `~/.claude2`. Default: the main session's. |
| `--cwd DIR` | Folder the task works in; by default the current folder. |

## Task statuses

`queued` (waiting for parents) → `held` (human gate) → `starting` → `running` → `done`, `needs-human`, `failed` or `stopped`. A `needs-human` task returns to `running` after `answer`.

## News that wait reports

| Line starts with | Meaning | What you do |
|---|---|---|
| `tN done` | Finished; `git: …`, `tab closes HH:MM` or `tab kept`, summary and outputs follow | Relay it; remind about uncommitted or unpushed work; offer `keep <id>`. |
| `tN needs-human` | The runner asks a question | Ask the user, then `answer`. |
| `tN held` | A `--gate human` task waits | Ask the user, then `release`. |
| `tN failed` | The runner or its launch failed; may carry `git: …` | Relay the reason; offer to retry with a new `spawn`. For `usage limit reached`, offer another `--agent` or `--claude-config`, and mention `quotaFallback` in the config. |
| `tN stalled` | The agent waits on a prompt in its pane | Tell the user to look at it (`focus`). |
| `tN turn-ended-unreported` | The runner stopped without reporting | `send` it a nudge to report, or tell the user. |
| `tN usage-limit` | The runner's screen shows its agent's usage limit and no `quotaFallback` agent is left | Tell the user; offer a new `spawn` with another `--agent` or `--claude-config`, or waiting for the reset. |
| `tN stopped` | Stopped by `stop` | Relay it. |
| `tN cleanable` | Its branch is merged and its worktree is clean | Ask the user, then `cleanup --yes <id>`. |
