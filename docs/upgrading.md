# Upgrading

```sh
hrk upgrade --check   # current and latest versions, and the release notes in between
hrk upgrade           # install the latest release
hrk upgrade --version v0.3.0   # install a named release, newer or older
```

`upgrade`:

1. asks the GitHub Releases API for the latest release, or the named one. Set `GITHUB_TOKEN` to raise the API's rate limit of 60 anonymous requests an hour;
2. refuses to go to an older release unless you name it with `--version`;
3. for a release that may hold incompatible changes, prints the release notes and asks first (`--yes` skips the question). That is a new major version, or before 1.0 a new minor version;
4. downloads the archive over HTTPS and checks it against the release's `checksums.txt`; on a mismatch nothing changes;
5. runs the new binary's `version --json` beside the old one, and refuses when its state schema is older than the state on disk, or when it would migrate that state while runners are still working;
6. renames the new binary over the old one, then runs the new binary's `setup`, which refreshes the skill and the hook. It sets up every folder named with `--claude-config DIR` (repeatable), the default one (`$CLAUDE_CONFIG_DIR`, else `~/.claude`), and every other `~/.claude*` folder that already holds the kit's skill, each once, and prints `refreshing <folder>` for each. Folders without the kit's skill are left alone. `HRK_NO_SETUP=1` skips `setup`.

Running runners are separate processes, so an upgrade interrupts nothing. Their `report` calls go to the same path and reach the new binary. Within one major version the [report contract](report-contract.md) stays backward compatible, so a runner started by the old version still reports correctly.

## State migrations

When a release changes the state schema, the new binary migrates the state the first time it loads it, under a lock:

- It refuses while any runner is still working (`starting`, `running` or `needs-human`), or a task file cannot be read, and names those tasks. Finish or `stop` them first. If the new binary is already installed, for example by running `install.sh` again, go back with `upgrade --version <previous>` or `HRK_VERSION=<previous>` and `install.sh`, finish the runners, then upgrade again.
- It first copies the state folder to `<state folder>.bak-<old schema>`, for example `~/.local/state/herdr-runner-kit.bak-1`. When that name is taken by an older backup, the new one gets a time suffix. The copy is written under a temporary name and renamed when complete.

An upgrade within the same schema needs no migration and is always allowed.

Do not go back to a release with report contract 2 while a runner that handed off (attempt 2 or later) is open: the older binary rejects `report handoff` and drops the task's `attempt`, so a report from the ended session would count again. Let those runners finish, or stop them, first. Do not go back to a release with report contract 1 while runners started by a contract 2 binary are open: the older binary keeps only the last question of a round and rejects `--decision`. See [report-contract.md](report-contract.md#compatibility).

A project record written by this release uses `manifestVersion: 2`, in the kit's state and in a project's committed `handbook/product/project.md`. Version 2 adds a gate outcome's `content` (the git object id of the approved spec or pack), a change request's `path`, and a milestone's `released` and `roadmapUpdate` (the git blob ids of the release notes and roadmap update each `milestone done` found). An older binary that reads only version 1 refuses such a record with "upgrade herdr-runner-kit" rather than an unknown field. Version 1 records are read as before and written back as version 2 at their next change. An older release cannot drive a project once its record is version 2, so do not go back to one while you drive projects. The id of a change request's impact runner stays in the kit's state and is never committed.

Upgrade the kit before you add a `pi` or `omp` agent to `config.yaml`, in a kind or in `quotaFallback`: an older binary refuses the whole config over an agent spec it does not know. Going back to such a release makes pi and omp tasks unreadable to it, so finish or stop them, and remove pi and omp from the config, first.

An older binary refuses a config key it does not know. Before going back to a release older than a key you set, such as `quotaFallback`, `keepFor`, `stallAfter`, `contextHandoff` or `contextHandoffAt`, remove that key from the config.

An older binary reads a task that a newer one wrote, since it ignores fields it does not know, but drops those fields when it rewrites the task: for example `roundHead` and the `git` fields `head`, `sinceStart` and `untrackedPlans`, the plan run fields `plan`, `phase`, `checkoutOf`, `phaseStart` and `phaseCheck`, and the handoff fields `attempt`, `handoffs` and `relaunched`. Finish or stop plan runs before going back to a release without `plan run`: an older binary would launch a waiting phase in the main checkout instead of the run's worktree. The per-task `progress.json` that the stall check and the context hooks write is ignored by older binaries. Claude runners started by an older binary have no compaction hooks in their settings, so they are not asked to hand off and compact as before. A Codex runner calls the installed binary at every turn end, so after the upgrade it can be asked once its footer shows little context left; the request spells out the full `report handoff` command, which the new binary accepts.

## Changes to know about

- **`wait --task` has its own cursor.** It no longer reads or saves the main session's cursor, so news of other tasks is no longer skipped for the main session, and it runs while the auto re-arm hook holds that cursor. The cursor is named from the sorted ids and starts where the main session's cursor stands. A script whose waits must share one cursor passes the same `--cursor NAME` to each. See [commands.md](commands.md#what-wait-reports).
- **Quiet news waits for the next wake.** A first-round `done` without decisions of a chain step whose next task has not started, or of a `plan run` phase, and `stopped` no longer wake the main session on their own, nor do `cleanable` and `processes-left` while other tasks run. Those two usually come when everything is already idle, and then wake it at once. They come first with the next line that wakes it, when everything is idle, when a `wait` you started times out, or at the latest 50 minutes after the main session was last active. Set `wakeOn: all` in `config.yaml`, or run `wait --all-news`, to wake for every line as before; `wait --task` does too. Telegram is unchanged. See [commands.md](commands.md#what-wait-reports).
- **Claude runners start without MCP servers and unused tools.** A Claude runner now starts with `--strict-mcp-config`, so your MCP servers, plugin servers and claude.ai connectors do not load in it, and its settings deny `Artifact`, `SendFeedback`, `ShareOnboardingGuide` and `ListAgents`, plus `AskUserQuestion` and `ScheduleWakeup` while its input is locked. This takes about 24k tokens off each call. Pass `spawn --mcp`, or set `kinds.<name>.mcp: true` in `config.yaml`, to keep the MCP servers; a runner started before the upgrade and relaunched after it starts without them. See [config.md](config.md#runner-tools-and-mcp).
- **Kinds `research` and `report` run Claude Sonnet.** They now start `claude:claude-sonnet-5-5:medium` instead of Claude Opus at high effort. Set `kinds.research.agent: claude:claude-opus-5-5:high` (or `kinds.report.agent`) in `config.yaml` to keep Opus, or pass `--agent` to one `spawn`.
- **Claude, pi and omp runner tabs are locked.** Text typed and submitted in a Claude, pi or omp runner's tab no longer reaches its agent; the tab names `hrk send <id>` instead. Steer runners from the main session or Telegram, and answer a `needs-human` question with `hrk answer`. Dialog keys, `esc` and built-in slash commands still work there. `!` bash mode still runs in a Claude tab, but pi and omp refuse it. Codex tabs stay open. Set `runnerInput: open` in `config.yaml` to type into tabs as before; it applies to runners spawned after the change. See [config.md](config.md#runner-input).
- **Kind `code` runs Claude Sonnet.** A `code` runner now starts `claude:claude-sonnet-5-5:medium` in a worktree instead of Codex. Set `kinds.code.agent: codex` in `config.yaml` to keep Codex, or pass `--agent codex` to one `spawn`.
- **The command is `hrk`.** `herdr-runner-kit` stays as an alias: `install.sh` links it to `hrk`, and an install upgraded from an older release keeps its `herdr-runner-kit` binary and gets an `hrk` link beside it from `setup`. With `HRK_NO_SETUP=1` the link waits until you run `herdr-runner-kit setup`; `doctor` names the missing link. Hooks and running runners call the binary by its full path, so nothing breaks. The repository, the skill, and the config and state folders keep the name `herdr-runner-kit`.
- **Kept tabs close.** A runner kept with `keep`, or by a project stage, now closes `keepFor` (4 hours by default) after its task finished, and running a stage again releases the stage's earlier kept tab. Set `keepFor: 0` to keep tabs until `keep --off` or `stop`, as before.
- **`show` prints compact text.** It no longer prints the task as JSON followed by text. Scripts that parsed it should use `show --json`, which prints the task as one JSON object; the prompt shows in the text view only with `--prompt`.
- **The `git:` text grew.** It ends with `head <sha>`, names the untracked share under `plans/`, and for a runner in the main checkout counts `N commits since start` instead of saying "no new commits". `tab kept` became `tab kept until HH:MM`.
- **Stalls.** A `working` agent with no visible progress for `stallAfter` (10 minutes by default) is nudged, then interrupted, then reported `stalled`; set `stallAfter: 0` to turn this off. A `stalled` line quotes the prompt, and Claude's self-denying safety prompt is no longer news unless it outlasts its timeout.
- **New news.** `processes-left` names processes a closed runner left running. A task whose herdr pane is gone after a restart now fails with `runner lost: herdr pane gone`.
- **Cleanup.** Squash- and rebase-merged branches count as merged, and `cleanup` without `--owner` includes project tasks.
- **Projects.** `project run pack --after …` waits for `release` unless `--gate auto`.
- **Context handoff.** A runner near its context limit is asked to hand off once to a fresh session of its task: Claude runners at their first automatic compaction, Codex runners at a turn end with `contextHandoffAt` percent (15) of the context left or less. Set `contextHandoff: false` to keep the old behaviour. The report contract is now version 3, with `report handoff` and `HRK_ATTEMPT`; Telegram tags and `needs-human` lines name the attempt from the second one on, such as `#t3a2`.
- **Plan runs.** `plan run <plan>` runs an AgentKit plan one runner per phase on one branch, and `wait` wakes for the new `plan-done` news. It writes the frontmatter `status:` lines of `plan.md` and the phase files.

## Update notes

`hrk version --check` asks GitHub right away. `doctor` also notes a newer release, but asks GitHub at most once a day and caches the answer in your user cache folder (`~/.cache/herdr-runner-kit/latest.json` on Linux). `list`, `report` and the other commands never touch the network. Set `updateCheck: false` in the config to turn the `doctor` note off.
