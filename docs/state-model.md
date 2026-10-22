# State model

This page is for contributors. It lists every piece of lifecycle state the kit persists, who may change it, and the rules it must keep. `runner.CheckInvariants` checks the rules on a snapshot of the store, under the names used here as headings; a change that touches a writer below should keep every rule it names.

`<S>` is the state folder: `HRK_STATE_DIR`, or by default `${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit`.

## Entities

| Entity | Stored in | States or key fields | Read or validated by |
|---|---|---|---|
| Task | `<S>/tasks/<id>/task.json`, written by `Store.Update` under `tasks/<id>/task.lock` | `queued`, `held`, `starting`, `running`, `needs-human`, `done`, `failed`, `stopped`; done, failed and stopped are terminal | `Store.Get`; `Store.Transition` never leaves a terminal status, `Store.Update` does not check |
| Task links | the same file | `after` (the only edge a follow-up walks), `checkoutOf`, `fixupOf`, `review.of`, `project` and `stage` (`<M>/<node>` for a milestone step), `node.{id,type,title,dependsOn}` (optional, written by `chain` after spawn), `worktree.{branch,base,path,branchCreated,removed,branchDeleted,offered,mergedTip,olderMergedTips}` | spawn does not check that linked tasks exist; `pendingParents` fails on a missing one |
| Task id sequence | `<S>/seq` under `seq.lock` | a counter | `Store.nextID` |
| Event log | `<S>/events.jsonl`, appended under a flock on the log | one JSON line per event | `ReadEventsSince` |
| Cursor | `<S>/cursor-<name>`, a byte offset; `wait` holds `cursor-<name>.lock` without blocking | offset | `InitCursor` |
| Main session markers | `<S>/main-sessions`, `<S>/main-projects-<hash>`, `<S>/main-active-<cursor>` | lists and mtimes | |
| Cleanup mark | `<S>/<hash>-cleanup` mtime under `cursor-<hash>-cleanup.lock` | time of the last check | |
| Plan run | no file of its own: the task fields `plan`, `phase`, `phaseStart`, `phaseCheck`, `review` and `fixupOf`; the `status:` line of `plan.md` and of each phase file; the marker `<S>/plans/plan-<hash>.done` | a phase is done when its newest task is done (`PhaseDone`) | `planrun.Read` |
| Project registry | `<S>/projects.json` under `projects.lock` | reserved, committed | `CheckProjectName` |
| Project record | `<S>/projects/<name>/record.json` under `<S>/project-locks/<name>/record.lock` | gates, milestones, changes, backlog, dependencies, `pending` | `project.Load`, `Record.Validate` (which does not check milestone, change or backlog statuses) |
| Milestone | `record.milestones[]` | `planned`, `in-progress`, `done`; `tag`, `closed`, `branch`, `features`, `released`, `roadmapUpdate` | `findMilestone` |
| Roadmap block | `discovery/roadmap-proposal.md` and `milestones/<M>/roadmap-update.md` in the project repository, read at HEAD | a yaml block | `ParseBlock`, `checkMilestones` |
| Milestone plan | `milestones/<M>/plan.md` in the project repository, read again at HEAD by every `chain` | nodes with `dependsOn` and `branch` | `ReadPlan`, `chainNodes`, `checkGraph` |
| Chain mapping | not stored: the newest task of stage `<M>/<node>` is that node's runner | | `PlanChain` |
| Change request, backlog item, dependency | `record.changes[]`, `backlog[]`, `dependencies[]` | change `open`, `now`, `next`, `backlog`, `rejected`; backlog `open`, `planned` | `DecideChange` |
| Merged tip | `worktree.mergedTip` and `olderMergedTips` on the task, and the refs `refs/hrk/merged/<id>[-<sha>]` | | `Worktree.AddMergedTip` |
| Rendered snapshot | `handbook/product/{project.md,decisions.md,backlog.md,roadmap.md}`, committed only at commit points | | only `project.md` is read back, by `Register` |

## Task transitions

"Guarded" means `Store.Transition` or `Store.SetStatus`, which never leave a terminal status. "Raw" means `Store.Update` setting the status itself. The last column says what the site does with the tasks that wait for this one.

| Site | From → to | Write | Waiting children |
|---|---|---|---|
| `Spawn` | new → queued, with its worktree in the same first write | `Store.CreateWith` | settles the new task once it exists |
| `Spawn` | new → failed, when the branch is taken | `Store.CreateWith` | none |
| `Spawn` | queued → held (human gate, no pending parent) | guarded | none |
| `beginLaunch` | queued, held → starting | guarded | none |
| `launch` | starting, needs-human → running, once the first prompt is typed | guarded; stops before the prompt when the task ended meanwhile | none |
| `waitForHuman` | starting ↔ needs-human, on a startup dialog | guarded | none |
| `report` | starting, running, needs-human → done, failed, needs-human | raw, refuses a terminal or waiting task | starts the detached follow-up (`StartAfter`, `hrk __after`), even when the event write fails |
| `Settle` | queued → starting, held, or done for a needless fixup; queued, held → failed | guarded | settles its own children when it fails the task |
| `skipFixup`, from `Settle` | queued → done, when the final review passed | guarded | plan-done in the same follow-up, and a follow-up of its own for what waits on the fixup |
| `Send` | done, failed → running (reopen) | raw | none |
| `Answer` | needs-human → running | guarded | none |
| `Stop` | any live → stopped | guarded | `ended` |
| `failLost` | running, needs-human → failed | guarded | `ended` |
| `fail`, called by `Launch`, `startLauncher`, `agentGone`, `CheckLaunch`, `relaunch` and `claimSwitch` | any live → failed | guarded | `ended` |
| `claimSwitch` | running, needs-human → starting, for a usage-limit fallback | raw, checks the status it read | none |
| `Handoff` | running → starting | raw, checks the status it read | none |

`Send` reopening a done or failed task is the only way out of a terminal status. Every other writer either moves a status through `Store.Transition`, which never leaves a terminal one, or checks the status it read under the task lock.

### The settle step

`Settle` (`internal/runner/report.go`) decides one waiting task from its parents in the store, and is the only code that does:

- A parent that failed, was stopped or does not exist fails the task, from queued or held, and settles the tasks that wait on it in turn.
- Once every parent is done, a queued task starts through a detached launcher, is held for release (the human gate, or a plan phase whose check holds), or, as a fixup the final review passed, is closed as done.
- Otherwise it changes nothing.

Each move is a guarded transition from queued, or held for a failure, so any number of callers may settle the same task, and a task starts at most once. They are:

- the follow-up of a parent that reported done (`startDependents`, which launches what it starts in its own process) or failed;
- `ended`, after `fail`, `Stop` and `failLost`: it settles the waiting children at once, and gives a plan task the detached follow-up, which checks whether the plan is done. That check never runs in the caller, because `plan run` holds the plan's lock while it spawns, and a spawn can fail a task;
- `Spawn`, once more after the task exists, for a parent that finished while spawn ran;
- the wait backstop's sweep, `SettleWaiting`, once a minute: it settles every task `waiting-task-has-a-path` finds stuck and emits plan-done for every run `plan-run-finishes` finds finished without it, leaving what finished within `SettleWindow` (2 minutes) to the follow-up on its way. A follow-up a crash or a reboot lost is recovered at the next `wait` that still has a live task.

A waiting task leaves only through `Settle`, `release` of a held task, or `stop`.

### The launcher and stop

The launcher's own writes (`launchUpdate`) notice a task that ended while it worked, such as one stopped before its pane was recorded. The launch records the tab it opened, types no prompt, and closes the runner through `closeRunner`.

A launcher that waits on a startup dialog the user must answer looks at the task every 5 seconds (`awaitAnswer`, `waitScreenClears`), so it returns within that time of a stop and quits the agent itself. Stop quits the agent with Ctrl+C, which is the exit of such a dialog; an agent herdr does not see, still starting or lost, gets the keys through the pane, up to three times, while a process other than the shell holds the pane's terminal. Either way `stopped-gets-no-agent` holds: no agent outlives a stop.

### Unreported turns

`Unreported` counts the turns since the last report, `send` or `answer` that ended without a report; those reset it, and so does a fallback or handoff. The turn-end hook adds one per such turn, and the progress watcher (`checkIdle`) adds one for an agent idle at its prompt for `stallAfter`, under a check that the count and the running status did not change since it read them. The first step is a nudge; the second is news (`turn-ended-unreported` from a turn end, `stalled` from the watcher), and nothing follows until a reset. The count changes no status.

### Other lifecycle fields

| Field | Written by |
|---|---|
| `after` | `newTask`, once; `milestone chain` replaces a stale waiting runner instead of relinking it |
| `worktree` | `Spawn`, in the task's first write, also on a task that failed because its branch was taken; that task records the branch without owning it |
| `worktree.base` | `Spawn`; `Settle` fills an empty base from the single parent's branch when it starts or holds the task, or from the default branch once cleanup deleted that branch as merged; a parent branch deleted unmerged fails the task |
| `worktree.path`, `branchCreated` | `launch` and the worktree it creates |
| `worktree.removed`, `branchDeleted`, `offered` | cleanup (`Cleanup`, `NoticeCleanable`) and `closeRunner`; never cleared, not even by a `send` reopen |
| `worktree.mergedTip` | `recordMergedTips` in cleanup, and `FinishMilestone` |

## Milestone and project transitions

| Entity | From → to | Site | Guard | Command |
|---|---|---|---|---|
| Milestone | none → planned | `applyRoadmap`, `adoptRoadmap` | `ParseBlock` | `approve pack`, `milestone start` with a waived pack, `milestone done` with a roadmap update |
| Milestone | planned → in-progress | `StartMilestone` | gates pass, no re-approval pending, none in progress, the one before is done | `milestone start` |
| Milestone | in-progress → done | `FinishMilestone` | still in progress, close runner done and not stale, tag free, main checkout clean, merge found | `milestone done` |
| Milestone | done → in-progress | `ReopenMilestone` | `checkReopen`, tag not on origin | `milestone reopen` |
| Milestone | done → in-progress | `undoDone`, which restores each milestone still as `done` wrote it and keeps one another command changed since | none | `milestone done` when the records commit fails |
| Done milestone features | duplicates dropped | `healMoved` on every `Load` | none | any command |
| Gate | open → approved, changes-requested, rejected | `Decide` | `checkNew`, `checkOrder` | `approve`, `changes`, `reject` |
| Change request | open → now, next, backlog, rejected | `DecideChange` | once only | `cr decide` |
| Backlog item | open → planned | `ApplyBacklogMoves` | | `milestone chain` |
| Project changes | restored | `undoGraduate`, which restores each change still as `graduate` wrote it and keeps one another command changed since | none | `graduate` when it fails |

`milestone start`, `chain`, `done`, `reopen` and `close` hold the project's `milestone` lock for their whole run. `approve`, `cr decide`, `backlog add`, `dep`, `decide`, `project run` and `graduate` do not take it.

### Chain

`PlanChain` maps each plan node to the newest task of stage `<M>/<node>`, and compares the `node` that task recorded with the plan. A done or running node keeps its runner, unless the plan changed its title or type: then chain is refused (stop a live runner and chain again; give a done node a new id). A waiting runner is kept only while it matches its node, waits for exactly the runners of its node's parents and has the node's branch; otherwise it is stopped and replaced (`ChainStep.Replace`). A failed or stopped node is spawned again, on `<branch>-r<k>` when its branch is taken (`PickBranch`), based on its old branch only while its parents are done. The branches of kept and live nodes are reserved, so no two nodes share one. The close runner is queued after the final integrate runner with no explicit base (`CloseStep`); like a feature child with a single parent, it takes its parent's branch when it starts.

## Shared files and locks

A file that two processes can rewrite is rewritten under one rule:

1. A read-modify-write holds a lock on something that is never renamed or removed while it is held. It reads the file inside the lock, writes it with `fsutil.WriteFileAtomic` (a temporary file renamed over it, keeping the mode and writing through a symlink to its target), and releases the lock. `fsutil.Update` does all of this.
2. The lock lives with the owner of the file:
   - a kit state file has a sidecar `*.lock` in the state folder (`task.lock`, `seq.lock`, `projects.lock`, `main-sessions.lock`, `main-projects-<hash>.lock` and the others);
   - a file in a folder another program owns (`~/.claude.json` or `$CLAUDE_CONFIG_DIR/.claude.json`, `<claude config>/settings.json`, `<codex home>/config.toml`) is locked by a flock on that folder, so no lock file is left in the user's folders;
   - a repository file the kit rewrites uses the store lock that already guards it: the plan lock for `plan.md` and phase files, the project's `checkout` lock for handbook renders.
3. When a program outside the kit also writes the file (Claude Code, Codex), `fsutil.Update` reads it again after the write, inside the lock, and checks that the change landed. If not, it runs the whole attempt again, at most 3 times, and then returns an error that names the file. The locks are advisory, so they order the kit against itself; against Claude Code and Codex the check is the only guard.
4. Create-if-absent (`config.yaml`, the Telegram `access.json`) uses `fsutil.CreateExclusive`: a finished temporary file is linked to the target, which fails when the target exists. Never stat, then write.
5. Lock order: for a project, `milestone`, then `checkout`, then `record`; for a plan, the plan lock, then a task's `phase-check.lock`. `branch-base.lock` comes after those: spawn holds it from reading a task's parents until the task is written, and cleanup from reading the waiting tasks until it records a branch deleted, so a task never starts from a branch cleanup deleted after it chose it. A task's `task.lock` is never held while another lock is taken, and a folder lock is never nested. A project's lock files live in `<S>/project-locks/<name>/`, outside the project's folder, and are never removed; `ForgetProject` takes the three project locks in that order before it removes the project's folder.

`acceptPhase` writes a phase file's `status:` without the plan lock. `plan run` holds that lock while a spawn settles a done parent through `acceptPhase`, and `planDone` holds it while it calls `acceptPhase`; a flock is not reentrant, so taking it there would hang. The kit only writes `completed` to a phase file, and the write is atomic, so a reader never sees a cut file and two kit writers agree.

## Invariants

A rule marked *liveness* holds only once the follow-ups are done: it skips a task whose parent finished within the settle window or is in `InFlight` (see [The checker](#the-checker)). A rule marked *needs git* reads the repository, so `CheckInvariants` does not check it.

### Task rules

#### waiting-task-has-a-path

*Liveness.* A queued task has at least one parent that is not terminal. A queued or held task has no parent that failed, stopped or does not exist. No task is queued without a parent.

#### worktree-recorded

A task with `useWorktree` has a `worktree` with a branch from its first write.

#### stopped-gets-no-agent

A task stopped before its pane is recorded never gets a tab, an agent or a prompt. In a snapshot: a stopped task with a pane has its runner closed, or a close in progress.

#### terminal-stays-terminal

A terminal task leaves its status only through `send` reopening a done or failed task. A reopened task is running, or past running needs a human or starts a fallback; it never waits again. Checked against `Prev`.

#### fields-match-status

`finishedAt` is set exactly when the task is terminal. A live task holds `pendingDone` only while running and `question` only while it needs a human; a terminal task keeps the last of each.

#### links-resolve

Every id in `after`, `checkoutOf`, `fixupOf` and `review.of` names an existing task. `after` names neither the task itself nor a task that waits for it.

#### one-live-runner-per-step

At most one task is live per project stage, per plan phase, and per plan's final review.

#### plan-run-finishes

*Liveness.* Once every task of a plan run is terminal and the newest task of every phase is done, plan-done was emitted for that set: the plan's marker holds those phase tasks and the newest final review and fixup.

### Chain and milestone rules

#### child-follows-current-parents

After `chain` returns without error, the `after` of each queued chain task names the newest runners of its node's parents in the plan at HEAD, plus the main-checkout ids `spawnStage` adds. Without git the checker checks the part it can see: a queued chain task waits for no runner that a newer runner of the same step replaced.

#### base-is-a-parent-branch

When a waiting chain task's explicit `worktree.base` is the branch of another task of its project, it waits for that task, and that task is the newest runner of its stage.

#### base-branch-exists

*Needs git.* The base branch of a waiting task, or the branch of its single parent when its base is empty, exists until the task starts.

#### done-milestone-frozen

A done milestone's title, planned date, features, tag, closed date and branch change only through `milestone reopen`. A feature that `healMoved` drops because a later milestone that is not done lists it counts as moved, not changed. Checked against `Prev`.

#### milestone-order

Each milestone status is planned, in-progress or done. At most one milestone is in progress. Every milestone before one in progress or done is done.

#### feature-in-one-milestone

Each feature id appears in at most one milestone.

#### rollback-undoes-only-itself

A failed command's rollback reverts only the fields that command set. A single snapshot cannot show this; tests check it on the command that rolls back.

#### done-milestone-has-release

A done milestone has a tag, a closed date, released notes and a done close runner. That the tag exists *needs git*.

#### done-milestone-has-no-live-runner

No task of a done milestone's steps is live.

#### chain-matches-plan

*Needs git.* Every live chain task's stage names a node of the milestone plan at HEAD, or the kit's close or final integrate step.

### Cleanup rules

#### cleanup-spares-live-work

Cleanup never removes a worktree or deletes a branch that a live task runs in, shares through `checkoutOf`, or will start from. The checker sees the recorded part: a removed worktree a live task shares, and a deleted branch a waiting task starts from.

#### live-task-has-checkout

The worktree of a live task was not removed.

#### merged-tip-has-ref

*Needs git.* Every recorded merged tip has its `refs/hrk/merged/` ref.

### News rules

#### cursor-within-log

Every cursor offset is at most the size of `events.jsonl` and falls on a line boundary. It reads files, so `CheckInvariants` does not check it.

### Accepted behaviour

These are not rules:

- A crash between the `cleanable` event and the `offered` write repeats the `cleanable` notice once.
- Wake delivery is at most once: the cursor is saved before the wake is shown.
- Cleanup may offer a close runner's release branch once it is merged, before `milestone done`; `done` finds the merge without the branch.

## The checker

`internal/runner/invariants.go` holds the checker:

```go
func (r *Runner) Snapshot() (Snapshot, error)
func CheckInvariants(s Snapshot) []Violation
```

- `Snapshot` reads every task, the record of every registered project, and every plan-done marker, and sets `Now`. The caller sets `Settle`, `InFlight` and `Prev`.
- `CheckInvariants` reads nothing but the snapshot: no git, no herdr, no files. It is cheap enough to run after every step of a test.
- `Settle` is the settle window. Tests use 0 once they have run every follow-up; checks of a live store use a window longer than a follow-up's normal run.
- `InFlight` names the parents whose follow-up has not run yet, for tests that hold follow-ups back.
- `Prev` is an earlier snapshot of the same store. Only `terminal-stays-terminal` and `done-milestone-frozen` read it, and they are skipped without it.
- The rules are a table in `invariants.go`, one function per rule, each with a comment that states it. Violations come sorted by rule, project and task.
- `SettleWaiting` runs `waiting-task-has-a-path` and `plan-run-finishes` on the tasks and plan markers alone, and repairs what they find through `Settle` and `planDone`. Nothing else repairs.
- `internal/runner/sequence_test.go` drives a store through random steps (spawn, launch, report, a gone agent, a passed launch deadline, a run or lost follow-up, stop, a stop during a launch, release, answer, send and the sweep) and checks every rule after each step. Its count follows `-quickchecks`, and `-seqseed` replays a failure; a failure prints the minimal steps.
