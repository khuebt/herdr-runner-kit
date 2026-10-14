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

1. Direction round: run {{skill "ak:frontend-design"}} "two or three clearly different visual directions for <product>, each applied to the one or two main screens, the ones most stories pass through: one self-contained HTML file per direction in {{.Discovery}}/design/directions/, no external requests, each with a short note on colour, type, density and component style". Then ask one question through `report needs-human`: pick one direction, combine directions (say which parts of each), or run one more round of samples; give your recommendation and name the files' paths{{if .Tools.Lavish}}, and suggest the user view them with `lavish-axi <file>`{{end}}. Stop there; the answer arrives in this session, and another round of samples repeats this step. Skip this step when the user's notes for this stage say to skip the direction round or give a design system to follow; then build on that design system.
2. Run {{skill "ak:frontend-design"}} "design tokens and visual direction for <product>: write {{.Discovery}}/design/DESIGN.md (colour, type, spacing, components, states) from the spec, the wireframes and the chosen direction". Record in DESIGN.md which direction was chosen and why, or the design system the notes gave.
3. Run {{skill "ak:frontend-design"}} "clickable HTML prototype of the main flows in {{.Discovery}}/prototype/, using the tokens in {{.Discovery}}/design/DESIGN.md; self-contained files, no external requests". Link the screens so the main flows can be clicked through from prototype/index.html.
{{- if and .Share.Stitch .Tools.Stitch}}
- The user switched Stitch sharing on and STITCH_API_KEY is set: you may run {{skill "ak:stitch"}} for screen variants. Never print the key.
{{- end}}
{{- if .Tools.Lavish}}
- lavish-axi is installed: in your done summary, offer the prototype for the user's local review with it.
{{- end}}
{{- if .Tools.Penpot}}
- A Penpot container is running: mention it in your summary only; do not upload to it.
{{- end}}

Commit design/, including design/directions/, and prototype/ ("docs(discovery): design tokens and prototype") and report done with DESIGN.md and prototype/index.html as --output. Feedback arrives in this session: revise, commit and report again.
