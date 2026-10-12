# Setup, doctor and uninstall

## setup

```sh
herdr-runner-kit setup
herdr-runner-kit setup --claude-config ~/.claude --claude-config ~/.claude2
```

`setup` makes the machine ready and prints each change. A second run prints `no changes`. It:

- writes `~/.config/herdr-runner-kit/config.yaml` with only `schemaVersion` when there is no config yet;
- creates the state folder;
- installs the main-agent skill into `<claude config>/skills/herdr-runner-kit/` (`SKILL.md` and `references/commands.md`);
- adds the auto re-arm Stop hook to `<claude config>/settings.json`;
- when Telegram credentials are configured, writes the Telegram plugin's `channels/telegram/access.json` with your chat on an allowlist, if the plugin has no access list yet.

`--claude-config` selects the Claude config folders, and can be repeated. Without it, setup uses `$CLAUDE_CONFIG_DIR`, else `~/.claude`.

### Skill files and your edits

Each installed file carries a marker line with the version that wrote it and a hash of its content. `setup` replaces a file only while that hash still matches. If you edited a file, setup leaves it as is, shows the difference and exits with an error. `--force` replaces it.

### The auto re-arm hook

The skill tells the main agent to keep `herdr-runner-kit wait` running in the background. Models forget, so `setup` also installs a Claude Code `Stop` hook with `asyncRewake`. After every main-agent turn it runs `wait` in the background. When news arrives, it wakes the agent with the news lines. When nothing is running, or another `wait` is already watching, it exits without a word.

- It runs only in the session started by `main --exec`, which sets `HRK_MAIN` to that session's process id. Other Claude sessions, and runners (which have `HRK_TASK` set), skip it, so none of them can take news meant for the main agent. The command `main` prints sets `HRK_MAIN=1`. The hook then runs too, but cannot tell when that session ends.
- It stops when the main session's process is gone, so a leftover hook never holds the news cursor.
- It names the binary by absolute path, and does nothing when that binary is gone. Run `setup` again after moving the binary.
- Claude wakes the agent when a hook exits with 2, which is also how a crashed Go program exits. So `__autoarm` exits with 3 on news, and the hook turns only that into 2. A crash never wakes the agent.
- Every other key and hook in `settings.json` stays in place. `--no-hook` removes the kit's hook. Setup does not lock `settings.json`, so avoid running it while another tool edits that file.

## doctor

```sh
herdr-runner-kit doctor [--claude-config DIR]…
```

`doctor` changes nothing. It prints `ok`, `warn` or `fail` for each check, with the fix for each problem, and exits 1 when a check fails:

- the binary on `PATH` is this one, the config loads, and the state schema is not newer than the binary;
- herdr is installed and its server is running;
- herdr's Claude and Codex integrations are current;
- `claude`, `codex` and `git` are installed, and the flags the kit passes still appear in their `--help`;
- Telegram credentials are found;
- for each Claude config folder:
  - the skill is installed by this version, and unedited;
  - the auto re-arm hook points at this binary;
  - herdr's state hook is registered in `settings.json`, not just present as a script;
  - the `--dangerously-skip-permissions` prompt has been accepted;
  - the Telegram plugin is installed, has an access list, and loads only in the main session (see [telegram.md](telegram.md)).

## uninstall

```sh
herdr-runner-kit uninstall [--claude-config DIR]…
herdr-runner-kit uninstall --purge
```

`uninstall` removes the skill files that still match what setup wrote, and the auto re-arm hook. Files you edited are kept and listed. `--purge` also deletes the kit's config folder and state folder after you confirm; `--yes` skips the question. The binary itself and the Telegram plugin's files are left in place.
