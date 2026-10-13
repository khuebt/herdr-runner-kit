---
stage: feature
title: Build a feature
kind: flow
requires: [pack]
milestone: true
code: true
outputs:
  - handbook/
---
Node {{.Node}} of {{.Milestone}} for project {{.Project}}, as planned in {{.PlanPath}}; the node's entry is in the block below. Build only this node.

1. Run {{skill "ak:cook"}} "node {{.Node}} of {{.PlanPath}}: build only that section, with its tests" --auto --skip-journal. Do not commit yet.
2. Run {{skill "ak:test"}} "node {{.Node}} of {{.Milestone}}" and fix what fails.
3. Commit the node in one commit ("feat({{.Milestone}}): {{.Node}} <short title>"). Then run {{skill "ak:code-review"}} <the full hash of that commit>, fix every finding that holds, run the tests again and commit the fixes.
4. Update the handbook file that owns what you changed, such as handbook/foundation/architecture.md for a new interface, and commit it.
5. A dependency that is not ready, listed at the end of your rules, goes behind an adapter with a fake, so the node works and its tests pass; say so in the summary.

Report done with the handbook files you changed as --output.

{{.NodeBlock}}
