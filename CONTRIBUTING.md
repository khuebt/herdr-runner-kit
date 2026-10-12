# Contributing

Thanks for helping improve herdr-runner-kit. This guide covers the development setup, the checks every change must pass, and how changes become releases.

## Development setup

You need the Go version pinned in `go.mod` (the `toolchain` line) and Git. To run the kit itself you also need [herdr](https://herdr.dev) with Claude Code or Codex.

```sh
git clone https://github.com/khuebt/herdr-runner-kit.git
cd herdr-runner-kit
go build ./...
go test -race ./...
```

Install the linters used by CI:

```sh
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
go install github.com/goreleaser/goreleaser/v2@latest
```

## Checks

Run these before opening a pull request. CI runs the same checks.

```sh
go vet ./...
go test -race ./...
golangci-lint run
goreleaser check
```

To build a local snapshot of every release archive:

```sh
goreleaser release --snapshot --clean
```

## Commits and pull requests

- Pull requests are squash-merged. The **PR title** becomes the commit on `main`, and it must follow [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/), for example `feat: add gc command` or `fix(wait): never miss an event written during startup`. CI rejects other titles.
- Use `feat` for new behaviour, `fix` for bug fixes, `perf` for speed, and `docs`, `refactor`, `test`, `build`, `ci` or `chore` for the rest. Mark an incompatible change with `!`, for example `feat!: …`, and explain the migration in the PR body.
- Keep one logical change per pull request, and add or update tests for behaviour changes.
- Update the docs under `docs/` when a command, config key or file format changes.

## Releases

Releases are automated. See [docs/versioning.md](docs/versioning.md) for the version rules and the release flow. Do not edit `CHANGELOG.md` by hand; release-please generates it from the PR titles.
