---
stage: roadmap
title: Roadmap proposal
kind: report
requires: [spec]
keep: true
outputs:
  - "{{.Discovery}}/roadmap-proposal.md"
---
Roadmap for project {{.Project}}, from the approved {{.Discovery}}/spec.md and, when they exist, handbook/foundation/ and handbook/product/risks.md.

1. Run {{skill "ak:plan"}} "milestone roadmap for {{.Discovery}}/spec.md: M0 scaffold, then thin vertical slices M1..Mn, each with features, acceptance criteria, a test plan, an estimate and the features it depends on; no implementation phases" --fast --no-tasks --skip-journal. It writes under plans/; keep that folder uncommitted.
2. Move the result to {{.Discovery}}/roadmap-proposal.md: a short rationale, a timeline table from {{.Today}}, then the milestones. Each feature is a slice a single runner can build and test; dependencies point only at earlier features.
3. End the file with one fenced block tagged yaml that holds `milestones` (id, title, planned date, features with id, title and dependsOn) and `dependencies` (name, owner, milestone, workaround, secretLocation: where a secret is kept, never its value).

Commit only roadmap-proposal.md ("docs(discovery): roadmap proposal") and report done with it as --output.
