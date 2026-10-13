---
stage: spec
title: Write the spec and charter
kind: report
requires: []
keep: true
outputs:
  - "{{.Discovery}}/spec.md"
  - handbook/product/charter.md
---
Spec for project {{.Project}}. Start from {{.Discovery}}/brief.md and the inputs below.

Run {{skill "ak:brainstorm"}} "turn {{.Discovery}}/brief.md into a product spec" --report. Gather open questions into one round of up to 5 numbered questions, each with options and your recommendation, through `report needs-human`; then continue with the answers. Settle minor questions yourself and record them with --decision.

Write {{.Discovery}}/spec.md:
- front matter: `version: 1` (raise it by one on every revision), `type` (ui, cli, api, library or unknown), `track` (lite or standard);
- a change log, one line per version;
- problem; users; scope and non-scope; user stories, each with acceptance criteria; non-functional requirements; constraints; open questions.

Write handbook/product/charter.md: vision, users, success metrics, non-goals, quality bar, decision heuristics, and a "Working rules" section that later runners follow.

Commit both files ("docs(discovery): spec v<version>"), then report done with both as --output. The user approves the spec before any later stage runs; a revision request arrives in this session: revise, raise the version, commit and report again.
