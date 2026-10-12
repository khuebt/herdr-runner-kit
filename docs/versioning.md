# Versioning and releases

herdr-runner-kit versions three things independently. Each one changes only when its own contract changes.

| Version | What it describes | Where it lives |
|---|---|---|
| Product version | The released binary, following SemVer | git tags `vX.Y.Z`, `herdr-runner-kit version` |
| `schemaVersion` | The shape of config and state files | `internal/buildinfo`, and every config and state file |
| `contractVersion` | The `report` contract that runners follow | `internal/buildinfo`, `docs/report-contract.md` |

A new product version does not imply a new schema or contract version. A schema change always ships with a forward migration. Within one major product version, the report contract stays backward compatible. This lets a runner that started on an older binary still report after an upgrade.

## Bump rules

| Change | Before 1.0 | From 1.0 |
|---|---|---|
| Compatible fix, or a documentation correction | patch | patch |
| New compatible capability | minor | minor |
| Incompatible command, config, state or contract change | minor, with migration notes | major |

The project stays on 0.x until there is an explicit decision that the commands and file formats are stable. A published version or tag is never overwritten or moved.

## How a release happens

1. Pull requests are squash-merged. The PR title becomes the commit message, so it must follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/), which CI checks. `feat:` produces a minor bump. `fix:` and `perf:` produce a patch bump. A `!` after the type, or a `BREAKING CHANGE:` footer, marks an incompatible change.
2. On every push to `main`, release-please updates one open release PR. That PR holds the next version and the generated `CHANGELOG.md` entries.
3. Merging the release PR tags `vX.Y.Z` and creates the GitHub Release. GoReleaser then attaches Linux and macOS binaries for amd64 and arm64, plus `checksums.txt`.
4. Release candidates (`vX.Y.Z-rc.N`) are tagged by hand. They are built by the release-candidate workflow and marked as prereleases.

`CHANGELOG.md` is generated, so never edit it by hand. To change what a release says, fix the commit message in the PR title before merging.
