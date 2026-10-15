---
stage: close
title: Close the milestone
kind: flow
requires: [pack]
milestone: true
outputs:
  - handbook/
---
Close of {{.Milestone}} for project {{.Project}}{{if .MilestoneTitle}} (its title is in the block below){{end}}: prepare its release on your branch, which starts from the branch where the milestone's work was integrated.{{if .Started}} It started on {{.Started}}{{if .Planned}} and was planned for {{.Planned}}{{end}}.{{end}} The user merges your branch into {{.DefaultBranch}} through a pull request; the kit tags the release after it sees that merge.

1. Run {{skill "ak:docs"}} update for what {{.Milestone}} changed, and commit the handbook files it touched.
2. Run {{skill "ak:retro"}} {{.Started}}:<today as YYYY-MM-DD>. The date range is given, so ask the user nothing. It writes its report under plans/reports/: leave plans/ uncommitted and copy what matters into the release notes.
3. Read the learning numbers of the project's runners since the milestone started: `hrk events --learning --project {{.Project}} --since "$(( ($(date +%s) - {{.StartedUnix}}) / 3600 + 1 ))h"`.
4. Write {{.ReleaseNotesPath}}: the features delivered{{if ne .Milestone "M0"}} (from {{.PlanPath}} and the merged branches){{end}}, the change requests and backlog items below, the planned date against the actual one, the work moved to another milestone, the known debt, and the retro's highlights and learning numbers.
5. Only when work moved to another milestone, write {{.RoadmapUpdatePath}}: one line on what moved and why, then the only fenced block tagged yaml in the file. It holds `milestones` and no other key: the whole roadmap of handbook/product/roadmap.md, each milestone with its id, title, planned date (YYYY-MM-DD) and features (id, title, dependsOn), every done milestone unchanged and {{.Milestone}} still in it. The kit imports it when the milestone is done.
6. Commit the notes ("docs(release): {{.Milestone}} release notes").

Do not tag, merge, push or open a pull request. Report done with {{.ReleaseNotesPath}} as --output, and name your branch in the summary as the one to merge.
{{- if .MilestoneTitle}}

{{.MilestoneTitle}}
{{- end}}
{{- if .Changes}}

{{.Changes}}
{{- end}}
{{- if .Taken}}

{{.Taken}}
{{- end}}
