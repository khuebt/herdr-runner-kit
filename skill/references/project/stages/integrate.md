---
stage: integrate
title: Integrate features
kind: flow
requires: [pack]
milestone: true
code: true
outputs:
  - handbook/
---
Node {{.Node}} of {{.Milestone}} for project {{.Project}}: integrate the work it waited for on your branch, which starts from {{.DefaultBranch}}. The node's entry is in the block below; the branches to merge are in the results of the tasks you waited for.

1. Merge each of those branches into your branch with `git merge --no-edit <branch>`, one at a time. Resolve every conflict so both sides keep their intent, and commit each resolution.
2. Run {{skill "ak:test"}} "full test suite and lint of {{.Project}}" and fix what the merges broke. Commit the fixes.
3. When the suite or the lint cannot pass without changing what a feature is meant to do, stop and report failed, naming each failing check and the branches it comes from in the summary.

Do not merge into {{.DefaultBranch}}, push or open a pull request: the close runner prepares the release from your branch. Report done with the handbook files you changed as --output, or none.

{{.NodeBlock}}
