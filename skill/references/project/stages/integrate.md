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
Node {{.Node}} of {{.Milestone}} for project {{.Project}}: integrate the features it waited for, on a branch made from {{.DefaultBranch}}. The node's entry is in the block below; the branches to merge are in the results of the tasks you waited for.

1. Merge each of those branches into your branch with `git merge --no-edit <branch>`, one at a time. Resolve conflicts so both sides keep their intent, and commit each resolution.
2. Run {{skill "ak:test"}} "full test suite and lint of {{.Project}}" and fix what the merges broke. Commit the fixes.
3. When the suite cannot pass without changing a feature's intent, report failed with the failing checks in the summary.

Report done with the handbook files you changed as --output, or none.

{{.NodeBlock}}
