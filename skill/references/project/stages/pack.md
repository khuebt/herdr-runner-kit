---
stage: pack
title: Stakeholder approval pack
kind: report
requires: [spec]
keep: true
outputs:
  - "{{.ApprovalDir}}/"
graduated: true
---
{{- if .Graduated}}
Approval pack v{{.PackVersion}} for project {{.Project}}: what the stakeholders read before they approve the changed scope and timeline. The project graduated: build the pack from the committed {{.Spec}}, handbook/product/roadmap.md, the change requests in {{.ChangesDir}}/ and handbook/product/risks.md, using those that exist; the latest pack in {{.ApprovalDir}}/ shows what was approved before.
{{- else}}
Approval pack v{{.PackVersion}} for project {{.Project}}: what the stakeholders read before they approve scope, design and timeline. Build it from the committed {{.Discovery}}/spec.md, {{.Discovery}}/ux/, {{.Discovery}}/design/, {{.Discovery}}/prototype/, {{.Discovery}}/roadmap-proposal.md and handbook/product/risks.md, using those that exist.
{{- end}}

1. Run {{skill "ak:frontend-design"}} "one self-contained stakeholder approval pack in {{.ApprovalDir}}/pack-v{{.PackVersion}}/pack.html: inline CSS, images inlined as base64, no scripts or fonts from the network, no external requests; print-friendly A4 pages" with these sections:
   - summary: the problem, the users, and what approving commits to;
{{- if .Graduated}}
   - scope and non-scope, from the requirements, with what changed since the last approved pack and the change requests behind it;
   - mockups: the screens the change touches, as inlined images, when it touches any;
   - roadmap: milestones, a timeline from {{.Today}}, and estimates, from handbook/product/roadmap.md, done milestones marked as such;
   - risks and dependencies, each with an owner and a mitigation or workaround;
   - the version (v{{.PackVersion}}), the date, and the requirements it is built on.
{{- else}}
   - scope and non-scope, from the spec;
   - mockups: the main wireframes or prototype screens as inlined images;
   - prototype: a link to {{.Discovery}}/prototype/index.html in the repository, or an exported screen sequence;
   - roadmap: milestones, a timeline from {{.Today}}, and estimates, from the roadmap proposal;
   - risks and dependencies, each with an owner and a mitigation or workaround;
   - the version (v{{.PackVersion}}), the date, and the spec version it is built on.
{{- end}}
2. Check that pack.html opens offline: it must not reference http:// or https:// resources.
{{- if .Tools.PDF}}
3. Print it: `{{.Tools.PDF}} --headless --disable-gpu --no-pdf-header-footer --print-to-pdf="{{.Root}}/{{.ApprovalDir}}/pack-v{{.PackVersion}}/pack.pdf" "{{.Root}}/{{.ApprovalDir}}/pack-v{{.PackVersion}}/pack.html"`.
{{- else}}
3. No headless Chromium or Chrome was found on this machine: write pack.html, then report needs-human asking the user to install chromium (herdr-runner-kit doctor --tools checks it); once it is installed, print pack.pdf with chromium --headless --print-to-pdf before you commit.
{{- end}}
{{- if .Docx}}
{{- if .Tools.Soffice}}
4. The user asked for DOCX: `soffice --headless --convert-to docx --outdir "{{.Root}}/{{.ApprovalDir}}/pack-v{{.PackVersion}}" "{{.Root}}/{{.ApprovalDir}}/pack-v{{.PackVersion}}/pack.html"`.
{{- else}}
4. The user asked for DOCX, and LibreOffice (soffice) was not found: report needs-human asking the user to install it.
{{- end}}
{{- end}}
{{- if .Share.LavishHosted}}
- The user switched hosted Lavish sharing on: you may publish pack.html with it, and give the link in your summary.
{{- else}}
- Keep the pack on this machine: do not upload or host it anywhere. The user shares it.
{{- end}}

Commit only {{.ApprovalDir}}/pack-v{{.PackVersion}}/ ("docs(approval): pack v{{.PackVersion}}") and report done with pack.html and pack.pdf as --output. Never edit a pack folder that is already committed: when the user requests changes in this session, write the next version in a new pack-v<n> folder, commit it and report again.
