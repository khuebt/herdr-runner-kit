---
stage: ux
title: Flows and low-fi wireframes
kind: report
requires: [spec]
keep: true
outputs:
  - "{{.Discovery}}/ux/"
---
UX for project {{.Project}}, from the approved {{.Discovery}}/spec.md and handbook/product/charter.md.

1. Write the main user flows as mermaid diagrams in {{.Discovery}}/ux/flows.md, one per flow, each naming the stories it covers. Use {{skill "ak:mermaidjs-v11"}} for the syntax. Use {{skill "ak:excalidraw"}} with --export svg into {{.Discovery}}/ux/ only when the user's notes ask for Excalidraw.
2. Direction round: run {{skill "ak:frontend-design"}} "low-fi greyscale wireframes for <screens>: one HTML file per screen in {{.Discovery}}/ux/wireframes/, no art direction, no brand colours, placeholder text" for only the one or two main screens, the ones most stories pass through. Then ask one round through `report needs-human`: one question each on layout, navigation and information density, each with options and your recommendation, naming the sample files' paths{{if .Tools.Lavish}} and suggesting the user view them with `lavish-axi <file>`{{end}}. Stop there; the answers arrive in this session. Skip the questions, not the samples, when the user's notes for this stage say to skip the direction round or give a design system to follow.
3. Wireframe the remaining screens the flows need the same way, following the chosen direction or the notes.{{if ne .Type "ui"}} The project type is {{.Type}}: wireframe only the screens the spec really has, or write the command, request and output shapes as text instead.{{end}}
4. Write {{.Discovery}}/ux/README.md: the screen list, which story each screen serves, the direction chosen in the round, and the open questions.
{{- if .Tools.Lavish}}
- lavish-axi is installed: in your done summary, offer the wireframes for the user's local review with it.
{{- end}}
{{- if .Tools.Drawio}}
- draw.io is installed: add a .drawio copy of a flow only when the notes ask for one.
{{- end}}

Commit {{.Discovery}}/ux/ ("docs(discovery): ux flows and wireframes") and report done with ux/README.md as --output. Feedback arrives in this session: revise, commit and report again.
