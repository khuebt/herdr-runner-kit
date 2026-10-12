# Installation

herdr-runner-kit is one binary for Linux and macOS, on amd64 or arm64. It needs [herdr](https://herdr.dev), git, and Claude Code or Codex. `herdr-runner-kit doctor` checks all of them.

## One-line install

```sh
curl -fsSL https://raw.githubusercontent.com/khuebt/herdr-runner-kit/main/install.sh | sh
```

The script:

1. picks the archive for your OS and CPU from the latest [GitHub Release](https://github.com/khuebt/herdr-runner-kit/releases);
2. downloads it with the release's `checksums.txt` and checks the SHA-256 with `sha256sum` or `shasum -a 256`; on a mismatch it stops and installs nothing;
3. puts the binary in `~/.local/bin`, swapping it in by rename, so a copy that is running keeps working;
4. tells you when that folder is not on your `PATH`;
5. runs `herdr-runner-kit setup` (see [setup.md](setup.md)).

It never uses `sudo` and needs only `curl`, `tar` and a SHA-256 tool.

| Variable | Effect |
|---|---|
| `HRK_VERSION` | Install this release, such as `v0.2.0`, instead of the latest. |
| `HRK_INSTALL_DIR` | Install here instead of `~/.local/bin`. |
| `HRK_NO_SETUP=1` | Skip `setup`. Run it yourself later, for example with `--claude-config`. |
| `HRK_BASE_URL` | Download from another copy of the releases folder. Used by the install test. |

To set up more than one Claude config folder, install with `HRK_NO_SETUP=1` and run `herdr-runner-kit setup --claude-config ~/.claude --claude-config ~/.claude2`.

## From source

```sh
go install github.com/khuebt/herdr-runner-kit/cmd/herdr-runner-kit@latest
herdr-runner-kit setup
```

A source build reports its version as `dev`, so `doctor` does not look for updates.

## Checking the install

```sh
herdr-runner-kit doctor
```

It prints one line per check and the fix for each problem, and exits 1 when a check fails. On a machine without herdr, for example, it reports `fail herdr` with the install hint.

`scripts/test-install.sh` tests `install.sh` against a release built from the checkout and served on localhost, including a tampered checksum. CI runs it on Linux and macOS.
