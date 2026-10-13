---
stage: architecture
title: Architecture and test strategy
kind: report
requires: [spec]
keep: true
outputs:
  - handbook/adr/
  - handbook/foundation/
  - handbook/product/risks.md
---
Architecture for project {{.Project}}, from the approved {{.Spec}} and handbook/product/charter.md.

1. Run {{skill "ak:plan"}} "architecture and test strategy only for {{.Spec}}: stack, components, data model, interfaces, test pyramid, CI checks, definition of done; no implementation phases" --hard --no-tasks --skip-journal. It writes under plans/; keep that folder uncommitted.
2. Copy the result into:
   - handbook/adr/NNNN-<slug>.md, one ADR per decision (context, decision, consequences, alternatives);
   - handbook/foundation/architecture.md (components, data model, interfaces) and handbook/foundation/test-strategy.md (test pyramid, CI checks, definition of done);
   - handbook/product/risks.md (risk, likelihood, impact, mitigation).
3. Draw the system diagram with {{skill "ak:diagram"}} --format svg --out handbook/foundation/architecture.svg and link it from architecture.md.

Commit those paths ("docs(foundation): architecture and test strategy") and report done with architecture.md, test-strategy.md and risks.md as --output.
