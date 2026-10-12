# Commands

Every command prints short plain text, so the main agent's context stays small. `herdr-runner-kit <command> --help` lists every flag.

## Setup

| Command | What it does |
|---|---|
| `setup [--claude-config DIR]… [--force] [--no-hook]` | Installs the main-agent skill and the auto re-arm hook, and creates the config and state. A second run prints `no changes`. |
| `doctor [--claude-config DIR]…` | Checks every dependency and prints the fix for each problem. |
| `uninstall [--claude-config DIR]… [--purge [--yes]]` | Removes what `setup` installed; `--purge` also deletes config and state. |

See [setup.md](setup.md).

## Main session

Commands that take text (`spawn`, `send`, `answer`) also accept `--stdin`, so the main agent passes your words through a quoted heredoc instead of a shell string. The text may be up to 100 KiB, since the agent receives it as one command-line argument.

| Command | What it does |
|---|---|
| `spawn [flags] -- <prompt>` (alias `run`) | Records a task and returns at once. A detached launcher opens the runner in a new herdr tab, or a new worktree workspace, and sends the first prompt. |
| `wait` | Blocks until one of your tasks has news, prints one line per event and exits. Run it in the background and rerun it after each wake. |
| `answer <id> <text>` | Answers a runner that reported `needs-human`. It continues in the same session. |
| `answer --from-telegram --stdin` | Routes a Telegram reply by its `#id` tag, or to the only waiting task. See [telegram.md](telegram.md). |
| `main [--exec]` | Prints, or runs, the command that starts the main session with the skill and the Telegram channel. |
| `send <id> <text>` | Types a new prompt into a live runner. |
| `release <id> [--note TEXT]` | Opens a held task's human gate and starts it. |
| `list [--all]`, `show <id>` | Stored status and herdr's live state; one task in full. |
| `events [--since 1h] [--task ID] [--follow] [--timings]` | The event log, or the measured latencies per task. |
| `focus <id>` | Switches herdr to the runner's tab. |
| `stop <id> [--remove-worktree]`, `gc` | Quits runners and closes their tabs. Worktrees and branches are kept unless removal is asked. |

## Chaining

`spawn --after t1,t2` queues a task until every parent is done. The parents' summaries, outputs and worktree branches are added to the child's first prompt. A worktree child of a single worktree parent starts from that parent's branch unless `--base` says otherwise.

With `--gate auto` (the default) the child starts by itself within about a second of the last parent's report. With `--gate human` it moves to `held` and waits for `release`. A parent that fails or is stopped fails its waiting dependents.

## What `wait` reports

The main agent runs `wait` in the background. In the session `main --exec` starts, the auto re-arm hook that `setup` installs also runs it after every turn, so no news is missed when the agent forgets to rerun it.

`wait` wakes for `done`, `needs-human`, `failed`, `held`, `stalled`, `stopped` and `turn-ended-unreported`. Other events, such as `started`, only move its cursor.

```
t3 done · fix login · Login fixed; tests pass · out: /…/tasks/t3/out/notes.md
t4 needs-human · drop legacy API · Q: Drop the v1 endpoint? (recommended: yes)
```

- Reports that land within 200 ms of each other wake it once.
- Each line is at most 300 characters. `show <id>` has the full detail.
- It only sees tasks of its owner, the git top-level of the current folder by default, so several main sessions can wait at once. Each owner has its own cursor, and a second `wait` on the same cursor exits with `already waiting`.
- With nothing queued, starting or running, it exits with an `idle:` line that names the tasks waiting on you. It waits up to a second after the latest status change first, so news written right after that change is not lost.
- After 30 minutes without news it prints `timeout` and exits 0 (`--timeout` changes this).

While it runs, `wait` also watches each runner through herdr. An agent that exits without reporting fails its task. An agent blocked on a prompt in its pane is reported as `stalled`. A launch that never finishes fails its task.

## Runner side

Runners call `report`. See [report-contract.md](report-contract.md).
