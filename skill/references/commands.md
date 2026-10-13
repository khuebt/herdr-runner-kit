# herdr-runner-kit commands

Every command prints short plain text. `herdr-runner-kit <command> --help` lists every flag.

## Tasks

| Command | Use |
|---|---|
| `spawn [flags] --stdin` | Record a task and open its runner. Returns at once. |
| `list [--all]` | Your tasks and every registered project's, with stored status, herdr's live state, and TAB: `open` (with when `gc` closes it), `kept`, `closed` or `-`. `--all` shows every owner. |
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
| `events --learning [--task ID] [--project NAME]` | Per task: agents, attempts (starts plus reopening sends), nudges, sends, answers, agent switches, duration and time waited on a human after the spawn. |
| `sync <id> [--from REF] [--no-send]` | Merge origin's default branch (such as `origin/main`), or REF, into a worktree task's branch and ask the runner to re-run its checks. Refuses a busy agent, background work, an agent herdr cannot check, or a dirty worktree, and fetches only after those checks; a conflict is aborted and the text to `send` is printed. When it suggests `--from main`, local main has commits origin lacks. |

## Projects

| Command | Use |
|---|---|
| `new <path> (--brief FILE \| --stdin) [--remote URL] [--reference REPO] [--name NAME] [--allow-non-empty]` | Start a project: a git repository with the git-ignored original brief, the project record, one first commit and a herdr workspace. Never pushes. |
| `project status [name]` | Where a project stands and the next step. Reads only the kit's record and tasks. |
| `project register <path> [--name NAME]` | Register an existing git repository as a project. |
| `project forget <name> [--yes]` | Drop a project from the registry; the repository is left alone. |

A project's runners report to the owner `project:<name>`, and `wait` and `list` include every registered project, so a project's news reaches you from any folder.

### New project

1. Ask in one message for the folder, the remote URL (or none) and an optional reference repository to learn from. Never accept a URL that holds a password or token; `new` refuses it.
2. Brief with the exact `new` command and end with "Start?".
3. On yes, run it with the user's idea through `--stdin` and a quoted heredoc, or `--brief FILE` for a brief file:

   ```sh
   herdr-runner-kit new ~/code/demo-app --remote git@github.com:me/demo-app.git --stdin <<'EOF'
   <the user's idea, as given>
   EOF
   ```

4. Relay the output, including any `not added`, `not created` or `not registered` line. Tell the user the original brief stays on disk, untracked, and is never committed.

`new` refuses a folder that already holds files: ask the user before adding `--allow-non-empty`. For an existing git repository, run `herdr-runner-kit project register <path>` instead.

### Where are we

At the start of a session that touches a project, and when the user asks "where were we?" or "đến đâu rồi?", run `herdr-runner-kit project status <name>` and relay it. It reads only the kit's record and tasks, so it works with herdr down.

### Runners of a project

Spawn with `--project <name>`. The task works in the project's folder, its tab opens in the project's herdr workspace, and its news reaches you as `project:<name>`. A spawn whose folder (`--cwd`, else the current one) lies in a project is a project task without the flag. `events --learning --project <name>` shows the project's numbers.

### Telegram

- `meta: project status <name>`: the user sent `status <name>`. Run `herdr-runner-kit project status <name>` and reply with its output.
- `ambiguous: "status <name>" could be an answer for t3; …`: a task is waiting, so ask on Telegram whether the user meant the answer for that task (`#t3 status <name>`) or the project status.

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
| `--cwd DIR` | Folder the task works in; by default the current folder, or the project's folder with `--project`. |
| `--project NAME` | Registered project the task works for. It opens in the project's workspace and reports to `project:NAME`. Default: the project holding the task's folder, if any. |

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
