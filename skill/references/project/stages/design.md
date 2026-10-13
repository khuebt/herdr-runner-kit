---
stage: design
title: Design tokens and a clickable prototype
kind: report
requires: [spec]
keep: true
outputs:
  - "{{.Discovery}}/design/"
  - "{{.Discovery}}/prototype/"
---
Design for project {{.Project}}, from the approved {{.Discovery}}/spec.md, handbook/product/charter.md and the wireframes in {{.Discovery}}/ux/ when they exist.

1. Run {{skill "ak:frontend-design"}} "design tokens and visual direction for <product>: write {{.Discovery}}/design/DESIGN.md (colour, type, spacing, components, states) from the spec and wireframes".
2. Run {{skill "ak:frontend-design"}} "clickable HTML prototype of the main flows in {{.Discovery}}/prototype/, using the tokens in {{.Discovery}}/design/DESIGN.md; self-contained files, no external requests". Link the screens so the main flows can be clicked through from prototype/index.html.
{{- if and .Share.Stitch .Tools.Stitch}}
3. The user switched Stitch sharing on and STITCH_API_KEY is set: you may run {{skill "ak:stitch"}} for screen variants. Never print the key.
{{- end}}
{{- if .Tools.Lavish}}
- lavish-axi is installed: in your done summary, offer the prototype for the user's local review with it.
{{- end}}
{{- if .Tools.Penpot}}
- A Penpot container is running: mention it in your summary only; do not upload to it.
{{- end}}

Commit design/ and prototype/ ("docs(discovery): design tokens and prototype") and report done with DESIGN.md and prototype/index.html as --output. Feedback arrives in this session: revise, commit and report again.
