---
phase: 2
title: "API"
status: pending
dependencies: [1]
---

# Phase 2: API

## Context

Mentions `internal/elsewhere/x.go`, which is not in scope.

## Related Code Files

- Modify `internal/api/server.go:12-40`: the handler.
- Create `internal/api/` tests, and `docs/api.md:3`.
- Not paths: `Handler`, `go test ./...`, `/abs/path.go`, `../up.go`, `ak:cook`.

```sh
# a comment, not a heading
`internal/fenced.go`
```

### Notes

Inside the files section still: `internal/api/notes.go`.

## Steps

- `internal/steps.go` is outside the files section.
