# Commands

Every command prints short plain text, so the main agent's context stays small. `herdr-runner-kit <command> --help` lists every flag.

## Setup

| Command | What it does |
|---|---|
| `setup [--claude-config DIR]… [--force] [--no-hook]` | Installs the main-agent skill and the auto re-arm hook, and creates the config and state. A second run prints `no changes`. |
| `doctor [--claude-config DIR]…` | Checks every dependency and prints the fix for each problem. |
| `uninstall [--claude-config DIR]… [--purge [--yes]]` | Removes what `setup` installed; `--purge` also deletes config and state. |
| `upgrade [--version vX.Y.Z] [--check] [--yes] [--claude-config DIR]…` | Installs the latest release, or the named one, after verifying its checksum, then runs the new binary's `setup`. `--check` only shows what would change. It asks before a new major version, or before 1.0 a new minor one. |
| `version [--json] [--check]` | Prints the version, schema and contract. `--check` also asks GitHub for a newer release. |

See [setup.md](setup.md) and [upgrading.md](upgrading.md).

## Main session

Commands that take text (`spawn`, `send`, `answer`) also accept `--stdin`, so the main agent passes your words through a quoted heredoc instead of a shell string. The text may be up to 100 KiB, since the agent receives it as one command-line argument.

| Command | What it does |
|---|---|
| `spawn [flags] -- <prompt>` (alias `run`) | Records a task and returns at once. A detached launcher opens the runner in a new herdr tab, or a new worktree workspace, and sends the first prompt. |
| `wait` | Blocks until one of your tasks has news, prints one line per event and exits. Run it in the background and rerun it after each wake. |
| `answer <id> <text>` | Answers a runner that reported `needs-human`. It continues in the same session. |
| `answer --from-telegram --stdin` | Routes a Telegram reply by its `#id` tag, or to the only waiting task. See [telegram.md](telegram.md). |
| `main [--exec]` | Prints, or runs, the command that starts the main session with the skill and the Telegram channel. |
| `send <id> <text>` | Types a new prompt into a live runner. A `done` or `failed` task whose tab is still open becomes `running` again, and its next report wakes `wait`. A `stopped` task refuses it. |
| `release <id> [--note TEXT]` | Opens a held task's human gate and starts it. |
| `list [--all]`, `show <id>` | Stored status, herdr's live state, and the TAB column: `open` (with when `gc` closes it), `kept`, `closed`, or `-` before a tab exists; one task in full. |
| `events [--since 1h] [--task ID] [--follow] [--timings]` | The event log, or the measured latencies per task. |
| `events --learning [--since 1h] [--task ID]` | Per task: kind, agents, status, attempts (starts, plus sends that reopened a finished task), nudges, sends, answers, `needs-human` reports, unreported turns, agent switches, duration from spawn to last finish, and time waited on a human after the spawn. Every number comes from the event log. |
| `focus <id>` | Switches herdr to the runner's tab. |
| `stop <id> [--remove-worktree]`, `gc` | Quits runners and closes their tabs. `gc` closes those that finished at least 30 minutes ago (`--older-than`), except kept runners (see `keep`) and a runner whose agent is working or blocked because someone kept using its tab; `wait` does the same for its owner's runners while it watches, and the auto re-arm hook once nothing is left to watch. Worktrees and branches are kept unless removal is asked; `gc --remove-worktree` also closes kept runners. |
| `sync <id> [--from REF] [--no-send]` | Merges origin's default branch (such as `origin/main`), or `--from REF`, into the branch checked out in the task's worktree, then asks the runner to re-run its checks and report. See [Syncing a branch](#syncing-a-branch). |
| `keep <id> [--off]` | Keeps a finished runner's tab open until `keep --off` or `stop`. `--off` lets `gc` close it again, at once when its close time has passed. |
| `cleanup [id]... [--yes] [--no-fetch]` | Fetches origin, then lists your finished worktree tasks whose worktree or branch is still there, as `merged`, `unmerged`, `dirty` or `kept`. `--yes` removes the worktree and deletes the local branch of each merged one, or of the given ids. See [Cleaning up](#cleaning-up). |

## Chaining

`spawn --after t1,t2` queues a task until every parent is done. The parents' summaries, outputs and worktree branches are added to the child's first prompt. A worktree child of a single worktree parent starts from that parent's branch unless `--base` says otherwise.

With `--gate auto` (the default) the child starts by itself within about a second of the last parent's report. With `--gate human` it moves to `held` and waits for `release`. A parent that fails or is stopped fails its waiting dependents.

A Claude runner uses the main session's Claude config folder, and so its account. `spawn --claude-config DIR` picks another one, as a path or a bare name: `claude2` means `~/.claude2`, and `claude` means Claude's default, `~/.claude`. Run `setup` and `doctor` for that folder too, so its herdr hook and permission prompt are ready.

A worktree branch is named `hrk/<id>-<slug>` unless `spawn --branch NAME` names it after the repository's own convention, such as `fix/CSD-11-paid-leave`. The runner commits on that branch and never creates or switches branches. When a worktree task gets the default name, `spawn` also prints a `note:` with the repository's recent branch names, so the next spawn can follow them with `--branch`.

## Syncing a branch

`sync <id>` merges origin's default branch, such as `origin/main` where pull requests merged on the remote land, into a worktree task's branch, so a runner that fell behind can re-run its checks. It refuses while the task is starting, while its agent is busy, runs background work or cannot be checked through herdr, when the worktree has uncommitted changes, and when the worktree has left the task's branch. Only after these checks does it fetch origin, without pruning. The output names the ref merged; when the local default branch has commits origin lacks, it also suggests `--from main` (or the local branch's name) to merge those. A branch that already holds the ref is left as it is. A conflicting merge is aborted, the branch is unchanged, and `sync` prints the conflicting files and the text to hand the merge to the runner with `send`. A merge git refuses to start, for example because it would overwrite an untracked file, is reported as an error and changes nothing. `--no-send` merges without telling the runner. `sync` never pushes, rebases or touches another branch, and a runner cannot run it.

## Usage limits

When a runner's agent hits a usage limit, the task switches to the next agent in the config's [`quotaFallback`](config.md#keys) list. The new agent starts in the same tab and checkout, with the first prompt and a note to continue from the partial work. The event log gets an `agent-switched` event, and `events --learning` lists the agents the task went through.

Claude reports the limit itself: its turn ends in a `StopFailure` hook with a rate-limit error and its limit message. With no fallback left, or none configured, that task fails with `usage limit reached for <agent>`, and its waiting dependents fail too.

Codex only shows the limit on screen, as its own `■ You've hit your usage limit …` error line; the same words in the conversation or in a tool's output never count. Such a line switches the agent when a fallback is left. Otherwise the task is not failed: the runner is nudged as for any unreported turn, and the main session gets `usage-limit` news saying the runner appears to be at its usage limit.

## Cleaning up

Finished runners close on their own (see `gc`), but their worktrees and branches stay until you remove them. `cleanup` without `--yes` only reports, however old a worktree is:

- **merged:** the branch, and the commit the worktree has checked out, are in the default branch (`origin/HEAD`, else `origin/main` or `origin/master`), no tracked file has changes, and no untracked file lies outside `plans/`. `cleanup --yes` removes these. Untracked files under `plans/` and ignored files are deleted with the worktree; the list says how many. The worktree and branch are checked again right before removal. Only a branch the task created is deleted, and only while it is still merged; a branch another worktree has checked out is kept.
- **unmerged:** commits are not in the default branch. A branch merged with squash or rebase looks unmerged, since its commits never land as they are; delete it yourself.
- **dirty:** tracked files have changes, or untracked files lie outside `plans/`; the list names the first few. They are never removed.
- **kept:** the runner is kept open with `keep`. Run `keep --off <id>` first.

`cleanup --yes` never closes a runner whose agent is still working or waiting at a prompt; it skips that task, and a later run cleans it.

While `wait` runs, it fetches origin at most every 10 minutes and reports each newly merged worktree once as `cleanable` news, so the main agent can ask you. Nothing is removed until you agree.

## What `wait` reports

The main agent runs `wait` in the background. In the session `main --exec` starts, the auto re-arm hook that `setup` installs also runs it after every turn, so no news is missed when the agent forgets to rerun it.

`wait` wakes for `done`, `needs-human`, `failed`, `held`, `stalled`, `stopped`, `turn-ended-unreported`, `usage-limit` and `cleanable`. Other events, such as `started`, only move its cursor.

```
t3 done · fix login · git: on hrk/t3-fix-login, 1 commit, no upstream · tab closes 15:42 · Login fixed; tests pass · out: /…/tasks/t3/out/notes.md
t4 needs-human · drop legacy API · Q: Drop the v1 endpoint? (recommended: yes)
```

- A `done` or `failed` line carries `git:`, the state of the task's checkout when the runner reported: its worktree, or `repo state` for a task without one. It counts uncommitted and untracked files, and commits not pushed, behind, diverged from the upstream, or with no upstream.
- A `done` line says `tab closes HH:MM` when `gc` will close the runner, or `tab kept` after `keep`.
- Reports that land within 200 ms of each other wake it once.
- Each line is at most 300 characters. `show <id>` has the full detail.
- It only sees tasks of its owner, the git top-level of the current folder by default, so several main sessions can wait at once. Each owner has its own cursor, and a second `wait` on the same cursor exits with `already waiting`.
- When every task has finished, it exits with an `idle:` line. A task that is `needs-human` or `held` keeps it waiting, because the answer or release can come from another main session's Telegram or from the runner's tab. It waits up to a second after the latest status change first, so news written right after that change is not lost.
- After 30 minutes without news it prints `timeout` and exits 0 (`--timeout` changes this).

While it runs, `wait` also watches each runner through herdr. An agent that exits without reporting fails its task. An agent blocked on a prompt in its pane is reported as `stalled`. A launch that never finishes fails its task.

## Runner side

Runners call `report`. See [report-contract.md](report-contract.md).
