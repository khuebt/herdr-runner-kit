# State model

This page is for contributors. It lists every piece of lifecycle state the kit persists, who may change it, and the rules it must keep. `runner.CheckInvariants` checks the rules on a snapshot of the store, under the names used here as headings; a change that touches a writer below should keep every rule it names.

`<S>` is the state folder: `HRK_STATE_DIR`, or by default `${XDG_STATE_HOME:-~/.local/state}/herdr-runner-kit`.

## Entities

| Entity | Stored in | States or key fields | Read or validated by |
|---|---|---|---|
| Task | `<S>/tasks/<id>/task.json`, written by `Store.Update` under `tasks/<id>/task.lock` | `queued`, `held`, `starting`, `running`, `needs-human`, `done`, `failed`, `stopped`; done, failed and stopped are terminal | `Store.Get`; `Store.Transition` never leaves a terminal status, `Store.Update` does not check |
| Task links | the same file | `after` (the only edge a follow-up walks), `checkoutOf`, `fixupOf`, `review.of`, `project` and `stage` (`<M>/<node>` for a milestone step), `worktree.{branch,base,path,branchCreated,removed,branchDeleted,offered,mergedTip,olderMergedTips}` | spawn does not check that linked tasks exist; `pendingParents` fails on a missing one |
| Task id sequence | `<S>/seq` under `seq.lock` | a counter | `Store.nextID` |
| Event log | `<S>/events.jsonl`, appended under a flock on the log | one JSON line per event | `ReadEventsSince` |
| Cursor | `<S>/cursor-<name>`, a byte offset; `wait` holds `cursor-<name>.lock` without blocking | offset | `InitCursor` |
| Main session markers | `<S>/main-sessions`, `<S>/main-projects-<hash>`, `<S>/main-active-<cursor>` | lists and mtimes | |
| Cleanup mark | `<S>/<hash>-cleanup` mtime under `cursor-<hash>-cleanup.lock` | time of the last check | |
| Plan run | no file of its own: the task fields `plan`, `phase`, `phaseStart`, `phaseCheck`, `review` and `fixupOf`; the `status:` line of `plan.md` and of each phase file; the marker `<S>/plans/plan-<hash>.done` | a phase is done when its newest task is done (`PhaseDone`) | `planrun.Read` |
| Project registry | `<S>/projects.json` under `projects.lock` | reserved, committed | `CheckProjectName` |
| Project record | `<S>/projects/<name>/record.json` under `record.lock` | gates, milestones, changes, backlog, dependencies, `pending` | `project.Load`, `Record.Validate` (which does not check milestone, change or backlog statuses) |
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
| `Spawn` | new → queued | `Store.Create` | none |
| `Spawn` | queued → held (human gate, no pending parent) | guarded | none |
| `beginLaunch` | queued, held → starting | guarded | none |
| `launch` | starting, needs-human → running, once the first prompt is typed | guarded | none |
| `waitForHuman` | starting ↔ needs-human, on a startup dialog | guarded | none |
| `report` | starting, running, needs-human → done, failed, needs-human | raw, refuses a terminal or waiting task | starts the detached follow-up (`StartAfter`, `hrk __after`) |
| `startDependents` | queued → held | guarded | n/a |
| `failDependent` | queued, held → failed | guarded | fails its own children in turn |
| `skipFixup` | queued → done, when the final review passed | guarded | plan-done in the same follow-up |
| `Send` | done, failed → running (reopen) | raw | none |
| `Answer` | needs-human → running | guarded | none |
| `Stop` | any live → stopped | guarded | `failDependents` |
| `failLost` | running, needs-human → failed | guarded | `failDependents` |
| `fail`, called by `launch`, `Spawn`, `agentGone`, `CheckLaunch` and `relaunch` | any live → failed | guarded | none |
| `claimSwitch` | running, needs-human → starting, for a usage-limit fallback | raw, checks the status it read | none |
| `Handoff` | running → starting | raw, checks the status it read | none |

`Send` reopening a done or failed task is the only way out of a terminal status.

A waiting task starts in one of three ways: at spawn, when no parent is pending; from `startDependents`, in the detached follow-up of a parent that reported done; or by `release` of a held task. Nothing runs a follow-up again once it is lost.

### Other lifecycle fields

| Field | Written by |
|---|---|
| `after` | `newTask`, once; `relinkQueued` during `milestone chain` |
| `worktree.base` | `Spawn`, after `Create`; `startDependents` fills an empty base from the single parent's branch; `relinkQueued` |
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
| Milestone | done → in-progress | `undoDone`, which restores the whole milestone list it read | none | `milestone done` when the records commit fails |
| Done milestone features | duplicates dropped | `healMoved` on every `Load` | none | any command |
| Gate | open → approved, changes-requested, rejected | `Decide` | `checkNew`, `checkOrder` | `approve`, `changes`, `reject` |
| Change request | open → now, next, backlog, rejected | `DecideChange` | once only | `cr decide` |
| Backlog item | open → planned | `ApplyBacklogMoves` | | `milestone chain` |
| Project changes | restored | `undoGraduate`, which restores the whole change list it read | none | `graduate` when it fails |

`milestone start`, `chain`, `done`, `reopen` and `close` hold the project's `milestone` lock for their whole run. `approve`, `cr decide`, `backlog add`, `dep`, `decide`, `project run` and `graduate` do not take it.

### Chain

`PlanChain` maps each plan node to the newest task of stage `<M>/<node>`. A done or live node keeps its runner; a failed or stopped one is spawned again, on `<branch>-r<k>` when its branch is taken (`PickBranch`). `relinkQueued` points a queued child at the runners of its node's parents. The close runner is queued after the final integrate runner, with that runner's branch as its explicit base (`CloseStep`). A feature child with a single parent gets an empty base and takes the parent's branch when it starts.

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

A terminal task leaves its status only through `send` reopening a done or failed task. Checked against `Prev`.

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
