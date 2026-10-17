# herdr-runner-kit commands

Every command prints short plain text. `hrk <command> --help` lists every flag.

## Tasks

| Command | Use |
|---|---|
| `spawn [flags] --stdin` | Record a task and open its runner. Returns at once. |
| `list [--all]` | Your tasks and every registered project's, with stored status, herdr's live state, TAB (`open` with when `gc` closes it, `kept until HH:MM`, `kept`, `closed` or `-`), AGE and LAST, the age of the latest event. `--all` shows every owner. |
| `show <id> [--prompt]` | One task as compact text: status, times, tab, `usage:` of a Claude runner, worktree, `git:` line, summary, decisions, question, outputs, last 5 events. The prompt only with `--prompt`. |
| `show <id> --json [--events N]` | The task as one JSON object, or `{task, events}` with `--events`. Parse this, never the text view. |
| `peek <id> [--lines N]` | The runner's visible screen (default 30 lines), secrets hidden. Use it instead of `herdr pane read`. |
| `send <id> --stdin` | Type a new prompt into a live runner. A done or failed task runs again; a stopped one refuses. |
| `answer <id> --stdin` | Answer a `needs-human` question. The id may name an attempt, such as `t3a2`; one for an attempt that ended is refused. |
| `answer --from-telegram --stdin` | Route a Telegram reply by its `#id` tag. |
| `release <id> [--note TEXT]` | Start a `held` task. |
| `focus <id>` | Switch herdr to the runner's tab. |
| `stop <id>... [--remove-worktree]` | Quit runners and close their tabs, several ids in one call. Branches are always kept. A main-checkout task gets a note naming untracked files it may have left. |
| `gc [--kept]` | Close runners of tasks that finished 30 minutes ago or more, and kept ones past the config's `keepFor` (4h). `--kept` also closes kept runners past `--older-than`. |
| `keep <id> [--off]` | Keep a finished runner's tab open for `keepFor` (4h by default) instead of letting `gc` close it after 30 minutes. `--off` lets `gc` close it again. |
| `cleanup [id]... [--yes]` | List finished worktrees and branches left behind, yours and every project's: merged (squash and rebase merges included), unmerged or dirty. `--yes` removes the merged ones. |
| `events [--since 1h] [--task ID] [--timings]` | The event log, or measured latencies. |
| `events --learning [--task ID] [--project NAME]` | Use it when the user asks how much steering tasks took. Per task: agents, attempts (starts plus reopening sends), nudges, sends, answers, agent switches, duration, ACTIVE (time in working rounds), STALLED, IDLE-KEPT (a kept tab open after the finish), time waited on a human after the spawn, and UNITS of token usage. |
| `usage [id...] [--since 24h] [--owner DIR \| --all] [--by task\|owner] [--main] [--json]` | Use it when the user asks what tasks or the main session cost in tokens. Per task: calls, MAX-CTX, subagents and UNITS (input 1, cache read 0.1, 5-minute cache write 1.25, 1-hour write 2, output 5; not money), and a total. Claude runners only; others show `-`. `--main` adds main sessions. |
| `heavy -- <command> [args...]` | Run your own tests, lint, typecheck and builds this way too: it waits for one of `heavySlots` slots (2 by default) shared with every runner, then runs the command and exits with its code. |
| `heavy [--json]` | Who holds each heavy slot and who waits, with command and age. Use it when a runner, or your own check, seems to wait. |
| `sync <id> [--from REF] [--no-send]` | Merge origin's default branch (such as `origin/main`), or REF, into a worktree task's branch and ask the runner to re-run its checks. Refuses a busy agent, background work, an agent herdr cannot check, or a dirty worktree, and fetches only after those checks; a conflict is aborted and the text to `send` is printed. When it suggests `--from main`, local main has commits origin lacks. |

## Main session's own work

Each of your calls re-reads your whole context, about 200k tokens in a long session; a light agent's call carries 30k to 60k. So keep reading out of it:
- A lookup is at most two commands, such as one search and one read. A longer one goes to the `scout` skill when it is installed, else an `Explore` subagent with `model: sonnet` briefed with paths, else a runner. Never fork your context for it: a fork carries all of it.
- Never poll runners with `list`, `show`, `events`, `herdr pane read`, the tasks folder or `sleep`; `wait` or the hook brings their news. Use `list` and `show` when the user asks for status.
- A runner's screen: `peek <id>` (text). Never a screenshot: an image stays in your context until the next compaction.
- In the session `hrk main` started (`$HRK_MAIN` set), the auto re-arm hook runs `wait` after every turn; a `wait` of your own beside it only gets `already waiting`. One exception: after `/clear` in a resumed main session the hook stops, so run your own `hrk wait` in the background there until you restart with `main --exec`.

## Projects

| Command | Use |
|---|---|
| `new <path> (--brief FILE \| --stdin) [--remote URL] [--reference REPO] [--name NAME] [--allow-non-empty]` | Start a project: a git repository with the git-ignored original brief, the project record, one first commit and a herdr workspace. Never pushes. |
| `project status [name]` | Where a project stands and the next step. Reads only the kit's record and tasks. |
| `project register <path> [--name NAME] [--reference PATH]` | Register an existing git repository as a project. A committed record names its reference repository without a local path; `--reference PATH` gives it. |
| `project forget <name> [--yes]` | Drop a project from the registry; the repository is left alone. |
| `project run <stage> [--stdin] [--after t1] [--gate auto\|human]` | Start a discovery stage runner from the kit's template. Refuses a stage whose gate is not approved yet. A `pack` given `--after` is held for `release` unless `--gate auto`. |
| `project approve <spec\|pack> --by NAME [--note TEXT] [--waive-pack]` | Record the user's approval of the committed version and commit the project's records. |
| `project changes <spec\|pack> --by NAME --stdin` | Record that changes were requested and send the feedback to the stage's runner. |
| `project reject <spec\|pack> --by NAME --note WHY` | Record a rejection. |
| `project decide --stdin` | Keep one decision of the user's in the decision log. |
| `project cr decide <n> <now\|next\|backlog\|reject> [--blocking] [--note TEXT]` | Record the user's decision on a change request. |
| `project backlog add --stdin` | Keep an idea for later; no runner starts. |
| `project dep add <name> --owner user\|vendor\|runner [--milestone M2] [--secret-location WHERE] [--workaround TEXT]` | Record something the project waits on outside itself. |
| `project dep set <Dn> --status needed\|requested\|ready` | Change its status; `ready` lists the held tasks that wait for it. |
| `project share <stitch\|lavish-hosted> <on\|off>` | The user's opt-in to share work outside this machine. |
| `project milestone start <M>` | M0: the scaffold runner, then its close runner. Later milestones: the plan runner. |
| `project milestone chain <M>` | Spawn the feature, integrate and close runners from the committed plan. Run it again after a failed runner; done nodes are kept. |
| `project milestone done <M> [--merged-commit SHA]` | After the user merged `release/<M>` and pulled: commit the records and tag the release locally. The notes it finds must be the close branch's, not a release merged before a reopen. |
| `project milestone reopen <M> --reason WHY` | Put a done milestone back in progress and delete its local tag. |
| `project milestone close <M>` | A new close runner on `release/<M>-r<k>` after a reopen or a failed close. |
| `project graduate [--export DIR] [--yes]` | Remove the discovery folder in one commit and tag the commit before it locally. Without `--yes` it only checks. |

A project's runners report to the owner `project:<name>`, and `wait` and `list` include every registered project, so a project's news reaches you from any folder.

### New project

1. Ask in one message for the folder, the remote URL (or none) and an optional reference repository to learn from. Never accept a URL that holds a password or token; `new` refuses it.
2. Brief with the exact `new` command and end with "Start?".
3. On yes, run it with the user's idea through `--stdin` and a quoted heredoc, or `--brief FILE` for a brief file:

   ```sh
   hrk new ~/code/demo-app --remote git@github.com:me/demo-app.git --stdin <<'EOF'
   <the user's idea, as given>
   EOF
   ```

4. Relay the output, including any `not added`, `not created` or `not registered` line. Tell the user the original brief stays on disk, untracked, and is never committed.

`new` refuses a folder that already holds files: ask the user before adding `--allow-non-empty`. For an existing git repository, run `hrk project register <path>` instead.

### Where are we

At the start of a session that touches a project, and when the user asks "where were we?" or "đến đâu rồi?", run `hrk project status <name>` and relay it. It reads only the kit's record and tasks, so it works with herdr down.

### Discovery stages

`project status` names the next step; follow it. The stages are `intake`, `spec`, then, after the spec is approved, `ux`, `design` (after `ux` for a UI project), `architecture`, `roadmap` (after `architecture`) and `pack` (after `roadmap` and `design`). Chain them with `--after`, and pass the user's notes with `--stdin`. A stage other than a gate is a report the user may veto; it is never held, except a `pack` chained with `--after`: it waits so the user can review the earlier stages first. Ask the user, then `release` it. Before UX or prototype work, run `hrk doctor --tools` and tell the user which optional tools are missing.

The `ux` and `design` runners first sample the one or two main screens and ask one direction round through `needs-human`: layout, navigation and density for `ux`; for `design`, pick, combine or another round of at least two visual directions. Relay the sample paths and every option in one message, suggest viewing the samples with Lavish when it is installed, and pass the user's choice with `answer`. The round is a question, not a gate. Notes that ask to skip the round, or give a design system to follow, skip it.

Runners never share work outside this machine (Stitch, hosted Lavish) until the user runs `project share … on` themselves.

### Gates

Two gates stop the project: the spec and the stakeholder pack. Only the user passes them.

1. Spec done: present it and ask. Record the answer with `project approve spec --by <name>`, `project changes spec --by <name> --stdin` with the feedback, or `project reject spec --by <name> --note WHY`.
2. On a lite-track project, add `--waive-pack` to `approve spec` only when the user chooses to skip the pack; it is refused once the pack has an outcome. With the pack waived, M0 starts without a roadmap; before M1, run `project run roadmap`: the next `milestone start` imports the committed roadmap proposal.
3. Pack done: point the user to `pack.html` and `pack.pdf` in its output; the user shares them with the stakeholders. Record their outcome the same way with `pack`. Approving the pack imports the roadmap.
4. A gate that says the spec or pack changed since its approval is open again: present the changed artifact and record the user's new outcome.

### Change control

- A change the user asks for after the spec is approved: `echo "<the change>" | hrk project run change --stdin`. Its runner reports the class as `decided: class -> tweak`, `within-milestone` or `scope`, and the impact file as output.
- Ask the user, then `project cr decide <n> now|next|backlog|reject`. A tweak goes to the runner building that part with `send` instead. A scope change decided `now` while a milestone runs needs `--blocking`; one decided `now` or `next` after the pack was approved needs a new pack approval (`project run pack`), and the output says so.
- An idea for later: `project backlog add --stdin`. A decision the user made in chat: `project decide --stdin`.
- Something outside the project, such as sandbox keys: `project dep add`. Record where a secret is kept with `--secret-location`, never its value. Spawn runners that need it with `--gate human --reason "needs D<n>"`; when `project dep set D<n> --status ready` lists them, `release` each.
- These records reach the repository with the next gate outcome or milestone commit.

### Milestones

1. M0: brief the reference repository, the stack and the user's proposals, then `project milestone start M0`. It is refused until the pack is approved or waived, and while a scope change waits for a new pack approval.
2. M1 and later: `project milestone start M<n>` spawns the plan runner. Its `done` line lists `decided: …`, the open questions it answered with the recommended option: relay them first and give the user a turn to veto; `send` a change. Then `project milestone chain M<n>`. A chain refused for an open question without an answer: ask those questions in one round and `send` the answers to the plan runner.
3. A feature runner failed: `send` it the failure, or run `chain` again; done nodes are kept.
4. The close runner's `done` names `release/M<n>`. The user pushes it and merges it through a pull request; never merge or push for them.
5. When the user says it is merged: `project milestone done M<n>`, and relay the tag and the release notes. The tag is local; the output prints the push command for the user. After a squash or rebase merge, add `--merged-commit <sha>`.
6. A done milestone needs fixes: `project milestone reopen M<n> --reason WHY`, fix on the branch the release came from, then `project milestone close M<n>` for a new release branch, and the same merge and `done`. A failed close runner: `project milestone close M<n>` too.

A project spawn prints `note: N runners of <name> are active …` above the project record's `parallelWarning`: mention it once; nothing was blocked.

### Graduate

Suggest it once, after M1 is done. `project run graduate` writes the evergreen requirements and copies the approved pack and open change requests out of the discovery folder. Then run `project graduate`, which only checks, show its output, and add `--yes` on the user's word. Tell the user that graduation does not purge history: every committed discovery file stays reachable through the `discovery-archive` tag. After it, discovery stages are refused; `project run pack` still builds a new pack from the requirements after a scope change, and `approve pack` clears the re-approval.

### Runners of a project

Spawn with `--project <name>`. The task works in the project's folder, its tab opens in the project's herdr workspace, and its news reaches you as `project:<name>`. A spawn whose folder (`--cwd`, else the current one) lies in a project is a project task without the flag. `events --learning --project <name>` shows the project's numbers.

### Telegram

- `meta: project status <name>`: the user sent `status <name>`. Run `hrk project status <name>` and reply with its output.
- `ambiguous: "status <name>" could be an answer for t3; …`: a task is waiting, so ask on Telegram whether the user meant the answer for that task (`#t3 status <name>`) or the project status.
- `meta: user approves spec of <name>; confirm and run …`: the user sent `approve spec <name>` (or `pack`). Confirm with them, then run the printed command with `--by`.
- `ambiguous: t3 is held (needs D2); …`: a task held with `--reason` starts only on a tagged reply such as `#t3 ok`.

## Plans

`plan run <plan.md|dir>` runs an AgentKit plan (`plan.md` plus `phase-NN-*.md` files) one fresh runner per phase. Use it when the plan has phase files; a plan without them, or a small one, still gets one `spawn --kind flow --skill ak:cook -- <plan.md>` runner.

| Flag | Meaning |
|---|---|
| `--dry-run` | Print the waves (phase, dependencies, scope size, why a phase runs alone) and spawn nothing. Put this in the brief. |
| `--parallel N` | Most phases at once (default 2). Only phases with declared, disjoint write scopes run side by side. |
| `--branch NAME` | Branch of the run's worktree, after the repository's convention. Refused on a rerun that already has a checkout. |
| `--kind`, `--agent`, `--claude-config`, `--language`, `--skill` | As for `spawn`, for every phase runner (default kind `flow`, skill `ak:cook`). |
| `--gate human` | Hold every phase after the first for `release`. |

- The first phase opens the worktree; every later phase works in the same checkout and branch, so the user gets one branch and one pull request.
- When a phase is `done`, the kit checks its commits and the checkout against the phase's declared files and sets the phase file's `status: completed`. A phase that changed files outside its scope, or left uncommitted files, holds the next wave: `tN held · … (phase 3 changed files outside its scope: a.go; release to continue)`. Ask the user before `release`; they may prefer to `send` the runner a fix first.
- `plan-done` names the report `<plan dir>/reports/plan-run-<YYMMDD-HHMM>.md`, with each runner's claim next to the kit's check, and `N/N phases done, K verified in scope`. Relay both.
- After a failed or stopped phase, `plan run` again resumes: done phases are kept and the checkout is reused. It refuses while a phase is still running or held.
- Runners never edit plan status; the kit does.

## spawn flags

| Flag | Meaning |
|---|---|
| `--kind NAME` | `code`, `flow`, `fix`, `research`, `report`, or a kind from the user's config. |
| `--agent SPEC` | `claude`, `codex`, `pi` or `omp`, with optional `:model` and `:effort`. `pi` and `omp` require the model, as `provider/model`: `pi:openai-codex/gpt-6-luna:high`. |
| `--worktree`, `--no-worktree` | Override the kind's worktree choice. |
| `--branch NAME` | Name of the new worktree branch, following the repository's convention. |
| `--base REF` | Git ref a new worktree branch starts from. |
| `--skill NAME` | Skill to invoke first, such as `ak:cook`. |
| `--after t1,t2` | Start when every listed task is `done`. |
| `--gate auto\|human` | `human` holds the task for `release`. |
| `--reason TEXT` | With `--gate human`: why it waits, such as `needs D2`. A bare Telegram `ok` does not release it. |
| `--title TEXT` | Short title; by default it comes from the prompt. |
| `--language NAME` | Language of the runner's report summaries and questions, such as `Vietnamese`. Default: the config's `language`. |
| `--claude-config DIR` | Claude config folder (account) of a Claude runner: a path, or a name such as `claude2` for `~/.claude2`. Default: the main session's. |
| `--mcp` | Keep the user's MCP servers in a Claude runner, which starts without them. Only when the task needs an MCP tool. |
| `--cwd DIR` | Folder the task works in; by default the current folder, or the project's folder with `--project`. |
| `--project NAME` | Registered project the task works for. It opens in the project's workspace and reports to `project:NAME`. Default: the project holding the task's folder, if any. |

Picking an agent: the brief names each runner's harness, model and effort with one reason. Go below the kind's default for simple work, such as `--agent claude:claude-sonnet-5-5:low` for a doc edit, a rename or a one-line fix. Use a strong model and high effort only for multi-file design, root-cause hunts or live experiments, such as `--agent claude:claude-opus-5-5:high` for a cross-module refactor.

## Task statuses

`queued` (waiting for parents) → `held` (human gate) → `starting` → `running` → `done`, `needs-human`, `failed` or `stopped`. A `needs-human` task returns to `running` after `answer`.

## News that wait reports

Lines marked *quiet* never wake you alone while other tasks run: they come first with the next line that does, with `idle`, or at the latest 50 minutes after your last turn. `wait --all-news`, `wait --task` and `wakeOn: all` wake for them too.

| Line starts with | Meaning | What you do |
|---|---|---|
| `tN done` | Finished; *quiet* for a chain step whose next task has not started, or a `plan run` phase, when it has no `decided:`; `decided: …`, `git: …`, `tab closes HH:MM` or `tab kept`, summary and outputs follow | Relay `decided:` first, so the user can veto; remind about uncommitted or unpushed work; offer `keep <id>`. |
| `tN needs-human` | The runner asks a question, or a round: `N questions` and one numbered line each; `attempt N` when the runner continues in a fresh session after a context handoff | Ask the user, all of a round in one message, then `answer` once, such as `1a 2b`, the same way for any attempt. An answer refused as `for attempt N of tN, which ended`: ask the user again. |
| `tN held` | A `--gate human` task waits; `(reason)` when held with `--reason` | Ask the user, then `release`. |
| `tN failed` | The runner or its launch failed; may carry `git: …` | Relay the reason; offer to retry with a new `spawn`. For `usage limit reached`, offer another `--agent` or `--claude-config`, and mention `quotaFallback` in the config. For `runner lost: herdr pane gone` (herdr or the machine restarted), relay it and offer a new `spawn`; `show <id>` names the worktree and branch with the partial work. |
| `tN stalled` | `blocked on a prompt: …` quotes the prompt the agent waits on; `no progress for N min …` or `retrying for N min` means it stayed busy without progress after a nudge and an interrupt | Relay the quoted text as is; offer `focus <id>` or `peek <id>`. The runner stays open. Never chase `blocked`, `unstalled`, `nudged` or `interrupted` progress events. |
| `tN processes-left` | *Quiet.* Processes the closed runner started still run, with pids and commands | Name them to the user; never kill them yourself. |
| `tN turn-ended-unreported` | The runner stopped without reporting | `send` it a nudge to report, or tell the user. |
| `tN usage-limit` | The runner's screen shows its agent's usage limit and no `quotaFallback` agent is left | Tell the user; offer a new `spawn` with another `--agent` or `--claude-config`, or waiting for the reset. |
| `tN stopped` | *Quiet.* Stopped by `stop` | Relay it. |
| `tN cleanable` | *Quiet.* Its branch is merged and its worktree is clean | Ask the user, then `cleanup --yes <id>`. |
| `tN plan-done` | Every phase of a `plan run` is done; `out:` is the run report | Relay the verified count and the report path. |

## Context handoff

A runner near its context limit is asked once to hand off: it writes `out/handoff-a<N>.md`, runs `report handoff`, and attempt N starts in the same tab and checkout. The `context-low`, `handoff` and `processes-carried` events are progress, never news: do nothing. `show <id>` prints `context: attempt 2 (handed off from …) · 3 compactions`. Telegram tags name the attempt from the second on, such as `#t3a2`; pass such replies to `answer --from-telegram` unchanged. `contextHandoff: false` in the config turns it off.
