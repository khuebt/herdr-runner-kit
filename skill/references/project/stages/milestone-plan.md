---
stage: milestone-plan
title: Plan the milestone
kind: research
requires: [pack]
milestone: true
keep: true
outputs:
  - "{{.PlanPath}}"
---
Plan of {{.Milestone}} for project {{.Project}}, from the approved {{.Discovery}}/spec.md, handbook/product/roadmap.md, handbook/foundation/ and the code on {{.DefaultBranch}}.

1. Run {{skill "ak:plan"}} "{{.Milestone}} of {{.Project}}: one phase per feature of the roadmap list below, each a slice one runner builds and tests, with strict file ownership" --parallel {{if eq .Track "standard"}}--tdd{{else}}--no-tasks{{end}} --skip-journal. It writes plans/<timestamp>-<slug>/; leave that folder uncommitted.
2. Copy the result into {{.PlanPath}}, one section per node, and make sure that:
   - shared APIs, types and schemas come first, as a contract node the features that use them depend on;
{{- if eq .Type "ui"}}
   - every UI feature's acceptance criteria include "matches the approved prototype" under {{.Discovery}}/;
{{- end}}
   - backlog items that fit this milestone, and the debt that slows it, are triaged within a small refactor budget (about a tenth of the milestone); name the backlog items you take;
   - every open question is answered with your recommended option. Pass the most important as --decision "<question> -> <answer>" (up to 7); the user reads them before chaining.
3. End {{.PlanPath}} with the only fenced block tagged yaml in it. It holds `nodes` (id such as F1, type feature or integrate, title, dependsOn, and branch only when the repository's convention needs one), `openQuestions` (q, answer) and `backlog` (id of a backlog item, to: a node id), and no other keys. A feature depends on at most one node: when it needs two, add an integrate node over them and depend on that. Do not name a node plan or scaffold.

Commit only {{.PlanPath}} ("docs(milestone): {{.Milestone}} plan") and report done with it as --output. A revision request arrives in this session: revise, commit and report again.
{{- if .Features}}

{{.Features}}
{{- end}}
{{- if .Backlog}}

{{.Backlog}}
{{- end}}
