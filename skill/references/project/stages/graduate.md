---
stage: graduate
title: Prepare graduation
kind: report
requires: [pack]
keep: false
outputs:
  - "{{.Requirements}}"
  - "{{.NewChangesDir}}/"
  - "{{.NewApprovalDir}}/"
  - handbook/
---
Prepare project {{.Project}} for graduation. Afterwards the kit removes {{.Discovery}}/ from the tree in one commit and tags the commit before it {{.ArchiveTag}}, so every committed file there stays in git history. Never edit, move or delete anything under {{.Discovery}}/ yourself.

1. Write {{.Requirements}}: the product's evergreen requirements, summarised from the approved {{.Discovery}}/spec.md (spec {{.SpecVersion}}) and the change requests below whose status is now, next or done. Name spec {{.SpecVersion}} as its source, keep the spec's story and acceptance criteria structure, and link nothing under {{.Discovery}}/.
{{- if .PackVersion}}
2. Copy {{.ApprovalDir}}/pack-v{{.PackVersion}}/ to {{.NewApprovalDir}}/pack-v{{.PackVersion}}/ byte for byte (`cp -R`), and change nothing in the copy: the kit checks it is the approved pack.
{{- else}}
2. The pack gate was waived for the lite track, so there is no pack to keep.
{{- end}}
3. Copy the file of every change request below whose status is open, now or next from {{.ChangesDir}}/ to {{.NewChangesDir}}/ under the same name; the kit points the records at the copies.
4. Run {{skill "ak:scout"}} "links into {{.Discovery}}/ from *.md and *.html files outside it". In each such file under handbook/, except the pack copy, point the link at the evergreen doc that now holds its content ({{.Requirements}} for the spec), or replace it with plain text: `<file> (archived at tag {{.ArchiveTag}})`. Do not edit files outside handbook/: name them in your report summary, since graduation refuses while any link into {{.Discovery}}/ is left.
5. Commit only these files ("docs(product): requirements before graduation") and report done with {{.Requirements}} as --output.
{{- if .Changes}}

{{.Changes}}
{{- end}}
