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
Design for project {{.Project}}, from the approved {{.Discovery}}/spec.md, handbook/product/charter.md, the wireframes in {{.Discovery}}/ux/ and any design docs the project already has.

Work as a product designer with taste who judges their own work. These principles guide you; they are not a checklist:
- Read the brief, spec, UX and existing design docs first. Question the current solution and propose a better one where it falls short.
- UX and the product's goals come first. Choose style, layout, type, colour and composition freely, each for a reason you can state; screens need not share one layout. Avoid generic UI and the look of a common AI template.
- Keep it consistent through a design system, tokens and reusable components. When the project or the notes already have a design system, inherit and reuse it, and propose improvements where it lowers quality.
- Respect accessibility, responsive layouts and technical constraints. Never change business rules or drop a required requirement.
- References, suggestions only: Taste Skill (https://github.com/Leonxlnx/taste-skill), GetLayers (https://www.getlayers.ai/), Land-book (https://land-book.com/) and other well-designed products. Learn their principles; never copy. Read third-party skills and repos only to learn: never install them or run their scripts. Viewing public pages is fine; never send your designs off this machine.

1. Direction round: run {{skill "ak:frontend-design"}} "clearly different visual directions for <product>, as many as the problem needs and at least two, each applied to the one or two main screens, the ones most stories pass through: one self-contained HTML file per direction in {{.Discovery}}/design/directions/, no external requests, each with a short note on colour, type, density and component style". Then ask one question through `report needs-human`: pick one direction, combine directions (say which parts of each), or run one more round of samples; give your recommendation and name the files' paths{{if .Tools.Lavish}}, and suggest the user view them with `lavish-axi <file>`{{end}}. Stop there; the answer arrives in this session, and another round of samples repeats this step. Skip this step when the user's notes for this stage say to skip the direction round or give a design system to follow; then build on that design system.
2. Run {{skill "ak:frontend-design"}} "design tokens and visual direction for <product>: write {{.Discovery}}/design/DESIGN.md (colour, type, spacing, components, states) from the spec, the wireframes and the chosen direction". Record in DESIGN.md which direction was chosen and why, or the design system the notes gave.
3. Run {{skill "ak:frontend-design"}} "clickable HTML prototype of the main flows in {{.Discovery}}/prototype/, using the tokens in {{.Discovery}}/design/DESIGN.md; self-contained files, no external requests". Link the screens so the main flows can be clicked through from prototype/index.html.
4. Before reporting done, open the prototype in a browser, take screenshots at mobile, tablet and desktop widths, judge them, and fix what falls short.
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
