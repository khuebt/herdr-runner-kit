package project

import (
	"strings"
	"testing"
)

func render(t *testing.T, rec Record, path string) string {
	t.Helper()
	data, err := Render(rec, path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRenderSnapshot(t *testing.T) {
	got := render(t, fullRecord(), SnapshotPath)
	_, body, _ := strings.Cut(strings.TrimPrefix(got, "---\n"), "\n---\n")
	want := "\n" + header + `
# demo-app

- Type: ui
- Track: lite
- Reference: ../reference-app
- Spec gate: approved v2 by Lan on 2026-10-14
- Pack gate: waived on 2026-10-14 waived
- Re-approval needed: scope grew
- Milestone: M1 Reading lists (active)
- Graduated: 2026-10-14 v0.3.0

See [roadmap](roadmap.md), [decisions](decisions.md) and [backlog](backlog.md).
`
	if body != want {
		t.Errorf("body:\n%s\nwant:\n%s", body, want)
	}
	if !strings.HasPrefix(got, "---\nmanifestVersion: 1\nname: demo-app\n") {
		t.Errorf("front matter:\n%s", got)
	}
}

func TestRenderDecisionsBacklogRoadmap(t *testing.T) {
	rec := fullRecord()
	rec.Decisions[0].Text = "project\ncreated  here"
	if got, want := render(t, rec, DecisionsPath), header+"\n# Decisions\n\n- 2026-10-14: project created here\n"; got != want {
		t.Errorf("decisions:\n%s", got)
	}
	wantBacklog := header + `
# Backlog

- B1 [moved] Dark mode (to M2)

## Change requests

- CR1 [open] Export to CSV (scope, blocking, M1)

## Dependencies

- D1 [waiting] Mail API key, owner ops, needed by M1; workaround: log mails
`
	if got := render(t, rec, BacklogPath); got != wantBacklog {
		t.Errorf("backlog:\n%s", got)
	}
	if strings.Contains(render(t, rec, BacklogPath), "team vault") {
		t.Error("the backlog names where a secret is kept")
	}
	wantRoadmap := header + `
# Roadmap

## M1 Reading lists

- Status: active
- Planned: 2026-10-14
- Started: 2026-10-14
- Branch: release/M1

Features:

- F1 Add a list
- F2 Share a list (after F1)
`
	if got := render(t, rec, RoadmapPath); got != wantRoadmap {
		t.Errorf("roadmap:\n%s", got)
	}
}

func TestRenderEmptyRecord(t *testing.T) {
	rec := NewRecord("demo-app")
	for path, want := range map[string]string{
		DecisionsPath: "None yet.",
		BacklogPath:   "Empty.",
		RoadmapPath:   "No milestones yet.",
		SnapshotPath:  "- Spec gate: pending\n- Pack gate: pending\n",
	} {
		if got := render(t, rec, path); !strings.Contains(got, want) || !strings.Contains(got, "written by herdr-runner-kit") {
			t.Errorf("%s:\n%s", path, got)
		}
	}
	if _, err := Render(rec, "README.md"); err == nil {
		t.Error("rendered a file the kit does not own")
	}
}
