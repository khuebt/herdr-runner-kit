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
| `project run <stage> [--stdin] [--after t1]` | Start a discovery stage runner from the kit's template. Refuses a stage whose gate is not approved yet. |
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
   herdr-runner-kit new ~/code/demo-app --remote git@github.com:me/demo-app.git --stdin <<'EOF'
   <the user's idea, as given>
   EOF
   ```

4. Relay the output, including any `not added`, `not created` or `not registered` line. Tell the user the original brief stays on disk, untracked, and is never committed.

`new` refuses a folder that already holds files: ask the user before adding `--allow-non-empty`. For an existing git repository, run `herdr-runner-kit project register <path>` instead.

### Where are we

At the start of a session that touches a project, and when the user asks "where were we?" or "đến đâu rồi?", run `herdr-runner-kit project status <name>` and relay it. It reads only the kit's record and tasks, so it works with herdr down.

### Discovery stages

`project status` names the next step; follow it. The stages are `intake`, `spec`, then, after the spec is approved, `ux`, `design` (after `ux` for a UI project), `architecture`, `roadmap` (after `architecture`) and `pack` (after `roadmap` and `design`). Chain them with `--after`, and pass the user's notes with `--stdin`. A stage other than a gate is a report the user may veto; it is never held. Before UX or prototype work, run `herdr-runner-kit doctor --tools` and tell the user which optional tools are missing.

Runners never share work outside this machine (Stitch, hosted Lavish) until the user runs `project share … on` themselves.

### Gates

Two gates stop the project: the spec and the stakeholder pack. Only the user passes them.

1. Spec done: present it and ask. Record the answer with `project approve spec --by <name>`, `project changes spec --by <name> --stdin` with the feedback, or `project reject spec --by <name> --note WHY`.
2. On a lite-track project, add `--waive-pack` to `approve spec` only when the user chooses to skip the pack; it is refused once the pack has an outcome. With the pack waived, M0 starts without a roadmap; before M1, run `project run roadmap`: the next `milestone start` imports the committed roadmap proposal.
3. Pack done: point the user to `pack.html` and `pack.pdf` in its output; the user shares them with the stakeholders. Record their outcome the same way with `pack`. Approving the pack imports the roadmap.
4. A gate that says the spec or pack changed since its approval is open again: present the changed artifact and record the user's new outcome.

### Change control

- A change the user asks for after the spec is approved: `echo "<the change>" | herdr-runner-kit project run change --stdin`. Its runner reports the class as `decided: class -> tweak`, `within-milestone` or `scope`, and the impact file as output.
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

- `meta: project status <name>`: the user sent `status <name>`. Run `herdr-runner-kit project status <name>` and reply with its output.
- `ambiguous: "status <name>" could be an answer for t3; …`: a task is waiting, so ask on Telegram whether the user meant the answer for that task (`#t3 status <name>`) or the project status.
- `meta: user approves spec of <name>; confirm and run …`: the user sent `approve spec <name>` (or `pack`). Confirm with them, then run the printed command with `--by`.
- `ambiguous: t3 is held (needs D2); …`: a task held with `--reason` starts only on a tagged reply such as `#t3 ok`.

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
| `--reason TEXT` | With `--gate human`: why it waits, such as `needs D2`. A bare Telegram `ok` does not release it. |
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
| `tN done` | Finished; `decided: …`, `git: …`, `tab closes HH:MM` or `tab kept`, summary and outputs follow | Relay `decided:` first, so the user can veto; remind about uncommitted or unpushed work; offer `keep <id>`. |
| `tN needs-human` | The runner asks a question, or a round: `N questions` and one numbered line each | Ask the user, all of a round in one message, then `answer` once, such as `1a 2b`. |
| `tN held` | A `--gate human` task waits; `(reason)` when held with `--reason` | Ask the user, then `release`. |
| `tN failed` | The runner or its launch failed; may carry `git: …` | Relay the reason; offer to retry with a new `spawn`. For `usage limit reached`, offer another `--agent` or `--claude-config`, and mention `quotaFallback` in the config. |
| `tN stalled` | The agent waits on a prompt in its pane | Tell the user to look at it (`focus`). |
| `tN turn-ended-unreported` | The runner stopped without reporting | `send` it a nudge to report, or tell the user. |
| `tN usage-limit` | The runner's screen shows its agent's usage limit and no `quotaFallback` agent is left | Tell the user; offer a new `spawn` with another `--agent` or `--claude-config`, or waiting for the reset. |
| `tN stopped` | Stopped by `stop` | Relay it. |
| `tN cleanable` | Its branch is merged and its worktree is clean | Ask the user, then `cleanup --yes <id>`. |
