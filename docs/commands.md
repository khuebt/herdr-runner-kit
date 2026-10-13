# Commands

Every command prints short plain text, so the main agent's context stays small. `herdr-runner-kit <command> --help` lists every flag.

## Setup

| Command | What it does |
|---|---|
| `setup [--claude-config DIR]… [--force] [--no-hook]` | Installs the main-agent skill and the auto re-arm hook, and creates the config and state. A second run prints `no changes`. |
| `doctor [--claude-config DIR]… [--tools]` | Checks every dependency and prints the fix for each problem. `--tools` also lists the optional tools the project stage templates use when present; a missing one is never a failure. |
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
| `list [--all]`, `show <id>` | Your tasks and every project's, or every owner's with `--all`: stored status, herdr's live state, and the TAB column: `open` (with when `gc` closes it), `kept`, `closed`, or `-` before a tab exists; one task in full. |
| `events [--since 1h] [--task ID] [--follow] [--timings]` | The event log, or the measured latencies per task. |
| `events --learning [--since 1h] [--task ID] [--project NAME]` | Per task: kind, agents, status, attempts (starts, plus sends that reopened a finished task), nudges, sends, answers, `needs-human` reports, unreported turns, agent switches, duration from spawn to last finish, and time waited on a human after the spawn. Every number comes from the event log. |
| `focus <id>` | Switches herdr to the runner's tab. |
| `stop <id> [--remove-worktree]`, `gc` | Quits runners and closes their tabs. `gc` closes those that finished at least 30 minutes ago (`--older-than`), except kept runners (see `keep`) and a runner whose agent is working or blocked because someone kept using its tab; `wait` does the same for its owners' runners while it watches, and the auto re-arm hook once nothing is left to watch. Worktrees and branches are kept unless removal is asked; `gc --remove-worktree` also closes kept runners. |
| `sync <id> [--from REF] [--no-send]` | Merges origin's default branch (such as `origin/main`), or `--from REF`, into the branch checked out in the task's worktree, then asks the runner to re-run its checks and report. See [Syncing a branch](#syncing-a-branch). |
| `keep <id> [--off]` | Keeps a finished runner's tab open until `keep --off` or `stop`. `--off` lets `gc` close it again, at once when its close time has passed. |
| `cleanup [id]... [--yes] [--no-fetch]` | Fetches origin, then lists your finished worktree tasks whose worktree or branch is still there, as `merged`, `unmerged`, `dirty` or `kept`. `--yes` removes the worktree and deletes the local branch of each merged one, or of the given ids. See [Cleaning up](#cleaning-up). |

## Projects

A project is a git repository the kit keeps a record for, so a main session can drive it from any folder.

| Command | What it does |
|---|---|
| `new <path> (--brief FILE \| --stdin)` | Starts a project: creates the folder and a git repository, keeps the original brief git-ignored under `handbook/product/discovery/brief/original`, writes the project record, makes one first commit of exactly the files it wrote, opens a herdr workspace and registers the project. `--remote URL` adds origin and never pushes; `--reference` names a repository to learn from; `--name`, `--initial-branch`. |
| `project status [name]` | Where the project stands: stage, gates, milestone, runners, tasks waiting on you, and one next step. It reads only the kit's record and tasks, never herdr, and changes nothing. Without a name: the project holding the current folder, else the only one, else one line per project. |
| `project register <path> [--name NAME]` | Registers an existing git repository, importing a committed `handbook/product/project.md` when there is one. Nothing is written to the repository. It refuses a linked worktree; register the main checkout. |
| `project forget <name> [--yes]` | Removes the registry entry and the kit's record, never the repository. It refuses while the project has unfinished tasks or record changes not yet committed, unless `--yes`. |
| `project run <stage> [--stdin]` | Spawns a discovery stage runner from the kit's stage template; `--stdin` adds your notes as untrusted data. See **Discovery and gates** below. |
| `project approve\|changes\|reject <spec\|pack>` | Records a gate outcome for the committed version, with `--by` and `--note`, and commits the project's records. `changes` sends the feedback from stdin to the stage's runner. |
| `project share <stitch\|lavish-hosted> <on\|off>` | Records whether the project's runners may share work outside this machine. Off by default. |
| `project decide --stdin` | Keeps one decision of yours in the decision log. |
| `project cr decide <n> <now\|next\|backlog\|reject>` | Records your decision on a change request. See **Change control** below. |
| `project backlog add --stdin` | Keeps an idea as the next backlog item. |
| `project dep add <name>`, `project dep set <Dn> --status S` | Tracks what the project waits on outside itself. |
| `project milestone start <M>` | Starts a milestone: M0's scaffold runner and its close runner, or a later milestone's plan runner. See **Milestones** below. |
| `project milestone chain <M>` | Spawns the milestone's feature, integrate and close runners from its committed plan. |
| `project milestone done <M> [--merged-commit SHA]` | Records the milestone done once you merged its release branch, commits the records and tags it locally. |
| `project milestone reopen <M> --reason WHY`, `project milestone close <M>` | Puts a done milestone back in progress, deleting its local tag; spawns a new close runner after a reopen or a failed close. |
| `project graduate [--export DIR [--include-original]] [--yes]` | Removes the discovery folder from the tree in one commit and tags the commit before it. See **Graduation** below. |

`new` refuses `/`, your home folder or a folder above it, a folder that is a git repository (use `project register`), lies inside one or holds one, a folder that is, lies inside or holds a registered project, a non-empty folder without `--allow-non-empty`, a `.gitignore` that is not a regular file, a folder on the way to a file it writes that is a symbolic link, and a remote URL that holds credentials. With `--allow-non-empty`, an existing `.gitignore` is left unchanged and out of the first commit: the brief is ignored through `.git/info/exclude`, and the output prints the line to add to `.gitignore`. `new` checks that git ignores the brief before it commits. Nothing is deleted when a step fails; a remote, workspace or registry step that fails is reported as `not added`, `not created` or `not registered`. Runners cannot run `new`, `register` or `forget`. This check reads `HRK_TASK`, which spawn sets in a runner's tab; it guards against mistakes, not against a runner that sets out to get around it, so it is not a security boundary. A project stays `being set up` until `new` finishes; when `new` was interrupted, `project status` and `spawn` say so, and `project forget <name>` drops it. A later `new` with the same name takes over such a leftover once it is 10 minutes old and its folder holds no git repository.

**Discovery and gates.** `project run <stage>` renders the stage's template (`intake`, `spec`, `ux`, `design`, `architecture`, `roadmap`, `pack`, `change`, `graduate`) with the project's record and spawns its runner in the project; `project status` names the next stage. Two gates stop the project until you decide: the spec and the stakeholder pack. Every stage but `intake` and `spec` is refused until the spec is approved. `approve` reads the version from the committed artifact and refuses one that is uncommitted, changed or already decided. An outcome holds for the content it was recorded for, the spec file's git blob or the pack folder's tree: when that content changes under the same version, the gate opens again as changed since approval, and a new outcome replaces the old one, which the output and `decisions.md` say. Approving the pack imports the roadmap from the committed roadmap proposal: every text field of its yaml block becomes one line of at most 300 characters, a dependency's owner must be `user`, `vendor` or `runner`, dependencies match by name whatever the case, an import never clears a field you set, and done and running milestones keep their scope. A lite-track project may skip the pack only with `approve spec --waive-pack`, before the pack has an outcome; the spec gate is never waived. With the pack waived, the first `milestone start` that finds the roadmap proposal committed imports it the same way, with a decision that says so; until then only M0 may start, and M1 and later are refused with a pointer to `project run roadmap`. Dates in the records and `decisions.md` are your local dates; full times keep their UTC offset. Values that look like secrets are hidden in gate notes, `decide` entries and change request titles before they are recorded. Open change requests and dependencies not ready reach each runner's rules as a fenced block of untrusted data. The pack stage writes a self-contained `pack.html` and `pack.pdf` (DOCX with `--docx`) for you to share. Runners share nothing outside this machine, such as through Stitch or hosted Lavish, until you switch it on with `project share`.

**Change control.** A change after the spec is approved starts with `project run change --stdin`. Its runner writes the change request (CR) file and reports its class, `tweak`, `within-milestone` or `scope`, as `--decision "class -> <class>"`. `cr decide` then puts it into the running milestone (`now`), the next one, the backlog, or rejects it. The running milestone keeps its scope: a scope change decided `now` while a milestone runs needs `--blocking`, and one decided `now` or `next` after the pack was approved marks the pack as needing a new approval before the next milestone. `decide`, `cr decide`, `backlog add` and `dep` commit nothing; their records reach `handbook/product/` with the next gate outcome or milestone commit. A dependency records where its secret is kept (`--secret-location`), never the secret; a value that looks like one is refused. Runners that need it are spawned with `--gate human --reason "needs D<n>"`, and `dep set D<n> --status ready` lists them with the `release` command for each.

**Milestones.** A milestone starts after the pack is approved, or waived on the lite track, and after the milestone before it in roadmap order is done; `start` is refused while a scope change waits for a new pack approval. M0 builds the repository standard and an empty app, then its close runner. A later milestone starts with a plan runner whose plan answers its open questions with the recommended options and reports them as `decided:`; you may veto them before `chain`. `chain` reads the yaml block of the committed plan: a feature has at most one parent, an integrate node any number, and a final integrate node and a close runner are added after the leaves. Integrate nodes work on `integrate/<M>-<id>`, the close runner on `release/<M>`; when an existing branch inside such a name, such as `release/<M>/x`, keeps git from creating it, the runner gets `<name>-r<k>` and the output says so, and a branch named like a folder above it, such as `release`, is refused. Run `chain` again after a runner failed: done nodes are kept. The close runner writes the release notes on `release/<M>` and never merges, tags or pushes. You merge that branch through a pull request and pull; `milestone done` then finds the merge in the local default branch, imports the release's roadmap update unless an earlier `done` of the milestone imported the same file, commits the records and creates the annotated tag from the project record's `tagFormat` (`v0.{N}.0` by default) locally. After a squash or rebase merge, pass `--merged-commit`. Either way the release notes it finds must be those at the tip of the close runner's branch, locally or on origin, and not those of a release merged before a reopen. `reopen` deletes the local tag, refused once the tag is on origin; the merged release stays, and `close` prepares a new release branch `release/<M>-r<k>` after the fixes. The kit never merges into the default branch and never pushes.

**Parallel runners.** When more of a project's runners are active than the project record's `parallelWarning` (3 by default), a project spawn prints a `note:` and `project status` shows the count. Nothing is blocked.

**Graduation.** `project run graduate` writes `handbook/product/requirements.md`, keeps the approved pack in `handbook/product/approval/` and the open change requests in `handbook/product/changes/`, and rewrites links into the discovery folder. `project graduate` then checks and prints what it would do; `--yes` removes `handbook/product/discovery/` in one commit with the records and tags the commit before it `discovery-archive` (or `discovery-archive-<n>`), locally. It refuses until both gates passed, the discovery stages finished and the requirements are committed. Graduation does not purge history: every committed file stays reachable through the tag, and the untracked original brief stays on disk. If the commit is refused, the folder, the records and the export folder are put back as they were. After it, stages that work on the discovery folder are refused and the spec gate stays as decided. The pack stage still runs: after a scope change it builds the next pack from the requirements, `handbook/product/roadmap.md` and the change requests into `handbook/product/approval/pack-v<n>/`, and `approve pack` clears the re-approval that `milestone start` waits for.

**Owners.** Every task reports to an owner, and `wait`, `list` and the auto re-arm hook pick tasks by owner. A project task's owner is `project:<name>`. Any other task reports to the git top-level of the folder `spawn` runs in, or that folder outside a repository, even when `--cwd` points elsewhere. `spawn --owner` overrides both. `wait` and `list` take the current folder's owner plus every `project:` owner, so project news reaches the main session from any folder, including a project started after the wait began. They go by the task's owner, not the registry, so a `projects.json` that cannot be read stops only the project commands; `spawn` then warns once and starts a task without a project.

**Project tasks.** `spawn --project NAME` makes a project task; so does a spawn whose folder (`--cwd`, else the current one) lies in a registered project. It works in the project's folder unless `--cwd` names another place inside it. Its tab opens in the project's herdr workspace, which is opened again when you closed it; a worktree task creates its worktree from that workspace. When the workspace cannot be opened, the task opens where a task without a project would, and the event log gets a `project-workspace-fallback` event.

## Chaining

`spawn --after t1,t2` queues a task until every parent is done. The parents' summaries, outputs and worktree branches are added to the child's first prompt. A worktree child of a single worktree parent starts from that parent's branch unless `--base` says otherwise.

With `--gate auto` (the default) the child starts by itself within about a second of the last parent's report. With `--gate human` it moves to `held` and waits for `release`. `--reason TEXT` says why, such as `needs D2`; it shows on the `held` line, and a Telegram reply must then name the task to release it. A parent that fails or is stopped fails its waiting dependents.

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
- A `done` line lists the runner's own decisions as `decided: …` right after the title, and a round of questions spans a header and one numbered line per question. See [report-contract.md](report-contract.md#question-rounds).
- Reports that land within 200 ms of each other wake it once.
- Each line is at most 300 characters. `show <id>` has the full detail.
- It only sees tasks of its owners: the git top-level of the current folder and every `project:` owner by default, or only `--owner`. So several main sessions can wait at once. Each owner has its own cursor, and a second `wait` on the same cursor exits with `already waiting`.
- When every task has finished, it exits with an `idle:` line. A task that is `needs-human` or `held` keeps it waiting, because the answer or release can come from another main session's Telegram or from the runner's tab. It waits up to a second after the latest status change first, so news written right after that change is not lost.
- After 30 minutes without news it prints `timeout` and exits 0 (`--timeout` changes this).

While it runs, `wait` also watches each runner through herdr. An agent that exits without reporting fails its task. An agent blocked on a prompt in its pane is reported as `stalled`. A launch that never finishes fails its task.

## Runner side

Runners call `report`. See [report-contract.md](report-contract.md).
