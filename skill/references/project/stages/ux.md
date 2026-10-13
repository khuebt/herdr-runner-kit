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
2. Run {{skill "ak:frontend-design"}} "low-fi greyscale wireframes for <screens>: one HTML file per screen in {{.Discovery}}/ux/wireframes/, no art direction, no brand colours, placeholder text" for the screens the flows need.{{if ne .Type "ui"}} The project type is {{.Type}}: wireframe only the screens the spec really has, or write the command, request and output shapes as text instead.{{end}}
3. Write {{.Discovery}}/ux/README.md: the screen list, which story each screen serves, and the open questions.
{{- if .Tools.Lavish}}
4. lavish-axi is installed: in your done summary, offer the wireframes for the user's local review with it.
{{- end}}
{{- if .Tools.Drawio}}
- draw.io is installed: add a .drawio copy of a flow only when the notes ask for one.
{{- end}}

Commit {{.Discovery}}/ux/ ("docs(discovery): ux flows and wireframes") and report done with ux/README.md as --output. Feedback arrives in this session: revise, commit and report again.
