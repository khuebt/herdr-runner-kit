# Upgrading

```sh
herdr-runner-kit upgrade --check   # current and latest versions, and the release notes in between
herdr-runner-kit upgrade           # install the latest release
herdr-runner-kit upgrade --version v0.3.0   # install a named release, newer or older
```

`upgrade`:

1. asks the GitHub Releases API for the latest release, or the named one. Set `GITHUB_TOKEN` to raise the API's rate limit of 60 anonymous requests an hour;
2. refuses to go to an older release unless you name it with `--version`;
3. for a release that may hold incompatible changes, prints the release notes and asks first (`--yes` skips the question). That is a new major version, or before 1.0 a new minor version;
4. downloads the archive over HTTPS and checks it against the release's `checksums.txt`; on a mismatch nothing changes;
5. runs the new binary's `version --json` beside the old one, and refuses when its state schema is older than the state on disk, or when it would migrate that state while runners are still working;
6. renames the new binary over the old one, then runs the new binary's `setup`, which refreshes the skill and the hook. Pass `--claude-config DIR` (repeatable) to set up those folders instead of the default. `HRK_NO_SETUP=1` skips `setup`.

Running runners are separate processes, so an upgrade interrupts nothing. Their `report` calls go to the same path and reach the new binary. Within one major version the [report contract](report-contract.md) stays backward compatible, so a runner started by the old version still reports correctly.

## State migrations

When a release changes the state schema, the new binary migrates the state the first time it loads it, under a lock:

- It refuses while any runner is still working (`starting`, `running` or `needs-human`), or a task file cannot be read, and names those tasks. Finish or `stop` them first. If the new binary is already installed, for example by running `install.sh` again, go back with `upgrade --version <previous>` or `HRK_VERSION=<previous>` and `install.sh`, finish the runners, then upgrade again.
- It first copies the state folder to `<state folder>.bak-<old schema>`, for example `~/.local/state/herdr-runner-kit.bak-1`. When that name is taken by an older backup, the new one gets a time suffix. The copy is written under a temporary name and renamed when complete.

An upgrade within the same schema needs no migration and is always allowed.

## Update notes

`herdr-runner-kit version --check` asks GitHub right away. `doctor` also notes a newer release, but asks GitHub at most once a day and caches the answer in your user cache folder (`~/.cache/herdr-runner-kit/latest.json` on Linux). `list`, `report` and the other commands never touch the network. Set `updateCheck: false` in the config to turn the `doctor` note off.
