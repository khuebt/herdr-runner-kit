package project

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Paths of the files the kit renders, relative to the repository root. The
// batched commit stages nothing else.
const (
	SnapshotPath  = "handbook/product/project.md"
	DecisionsPath = "handbook/product/decisions.md"
	BacklogPath   = "handbook/product/backlog.md"
	RoadmapPath   = "handbook/product/roadmap.md"
)

// RenderedPaths lists every rendered file, in commit order.
var RenderedPaths = []string{SnapshotPath, DecisionsPath, BacklogPath, RoadmapPath}

// header marks a rendered file, so readers know edits belong in the kit.
const header = "<!-- written by herdr-runner-kit from its project record; change it through herdr-runner-kit, not by hand -->\n"

// Render returns the content of the rendered file at path, one of
// RenderedPaths.
func Render(rec Record, path string) ([]byte, error) {
	switch path {
	case SnapshotPath:
		return renderSnapshot(rec)
	case DecisionsPath:
		return renderDecisions(rec), nil
	case BacklogPath:
		return renderBacklog(rec), nil
	case RoadmapPath:
		return renderRoadmap(rec), nil
	}
	return nil, fmt.Errorf("%s is not a file the kit renders", path)
}

// renderSnapshot writes the whole record as front matter, which
// ParseSnapshot reads back, followed by a generated summary.
func renderSnapshot(rec Record) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(rec); err != nil {
		return nil, fmt.Errorf("encode project snapshot: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode project snapshot: %w", err)
	}
	b.WriteString("---\n\n")
	b.WriteString(header)
	fmt.Fprintf(&b, "\n# %s\n\n", rec.Name)
	fmt.Fprintf(&b, "- Type: %s\n- Track: %s\n", rec.Type, rec.Track)
	if rec.ReferenceRepo != "" {
		fmt.Fprintf(&b, "- Reference: %s\n", oneLine(rec.ReferenceRepo))
	}
	fmt.Fprintf(&b, "- Spec gate: %s\n- Pack gate: %s\n", gateLine(rec.Gates.Spec), gateLine(rec.Gates.Pack))
	if rec.Reapproval.Needed {
		fmt.Fprintf(&b, "- Re-approval needed: %s\n", orDash(oneLine(rec.Reapproval.Reason)))
	}
	if m := currentMilestone(rec); m != nil {
		fmt.Fprintf(&b, "- Milestone: %s %s (%s)\n", m.ID, oneLine(m.Title), orDash(m.Status))
	}
	if g := rec.Graduated; g != nil {
		fmt.Fprintf(&b, "- Graduated: %s %s\n", day(g.At), g.Tag)
	}
	fmt.Fprintf(&b, "\nSee [roadmap](roadmap.md), [decisions](decisions.md) and [backlog](backlog.md).\n")
	return b.Bytes(), nil
}

func renderDecisions(rec Record) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\n# Decisions\n\n")
	if len(rec.Decisions) == 0 {
		b.WriteString("None yet.\n")
	}
	for _, d := range rec.Decisions {
		fmt.Fprintf(&b, "- %s: %s\n", day(d.At), oneLine(d.Text))
	}
	return b.Bytes()
}

func renderBacklog(rec Record) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\n# Backlog\n\n")
	if len(rec.Backlog) == 0 {
		b.WriteString("Empty.\n")
	}
	for _, it := range rec.Backlog {
		fmt.Fprintf(&b, "- %s [%s] %s", it.ID, orDash(it.Status), oneLine(it.Text))
		if it.To != "" {
			fmt.Fprintf(&b, " (to %s)", it.To)
		}
		b.WriteString("\n")
	}
	if len(rec.Changes) > 0 {
		b.WriteString("\n## Change requests\n\n")
		for _, c := range rec.Changes {
			fmt.Fprintf(&b, "- %s [%s] %s", c.ID, orDash(c.Status), oneLine(c.Title))
			var notes []string
			if c.Class != "" {
				notes = append(notes, c.Class)
			}
			if c.Blocking {
				notes = append(notes, "blocking")
			}
			if c.Milestone != "" {
				notes = append(notes, c.Milestone)
			}
			if len(notes) > 0 {
				fmt.Fprintf(&b, " (%s)", strings.Join(notes, ", "))
			}
			b.WriteString("\n")
		}
	}
	if len(rec.Dependencies) > 0 {
		b.WriteString("\n## Dependencies\n\n")
		for _, d := range rec.Dependencies {
			fmt.Fprintf(&b, "- %s [%s] %s", d.ID, orDash(d.Status), oneLine(d.Name))
			if d.Owner != "" {
				fmt.Fprintf(&b, ", owner %s", oneLine(d.Owner))
			}
			if d.Milestone != "" {
				fmt.Fprintf(&b, ", needed by %s", d.Milestone)
			}
			if d.Workaround != "" {
				fmt.Fprintf(&b, "; workaround: %s", oneLine(d.Workaround))
			}
			b.WriteString("\n")
		}
	}
	return b.Bytes()
}

func renderRoadmap(rec Record) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("\n# Roadmap\n")
	if len(rec.Milestones) == 0 {
		b.WriteString("\nNo milestones yet.\n")
	}
	for _, m := range rec.Milestones {
		fmt.Fprintf(&b, "\n## %s %s\n\n", m.ID, oneLine(m.Title))
		fmt.Fprintf(&b, "- Status: %s\n", orDash(m.Status))
		for _, f := range []struct {
			label string
			at    time.Time
		}{{"Planned", m.Planned}, {"Started", m.Started}, {"Closed", m.Closed}} {
			if !f.at.IsZero() {
				fmt.Fprintf(&b, "- %s: %s\n", f.label, day(f.at))
			}
		}
		if m.Branch != "" {
			fmt.Fprintf(&b, "- Branch: %s\n", m.Branch)
		}
		if m.Tag != "" {
			fmt.Fprintf(&b, "- Tag: %s\n", m.Tag)
		}
		if len(m.Features) > 0 {
			b.WriteString("\nFeatures:\n\n")
		}
		for _, f := range m.Features {
			fmt.Fprintf(&b, "- %s %s", f.ID, oneLine(f.Title))
			if len(f.DependsOn) > 0 {
				fmt.Fprintf(&b, " (after %s)", strings.Join(f.DependsOn, ", "))
			}
			b.WriteString("\n")
		}
	}
	return b.Bytes()
}

// currentMilestone is the first milestone not done, else the last one.
func currentMilestone(rec Record) *Milestone {
	for i := range rec.Milestones {
		if rec.Milestones[i].Status != "done" {
			return &rec.Milestones[i]
		}
	}
	if n := len(rec.Milestones); n > 0 {
		return &rec.Milestones[n-1]
	}
	return nil
}

func gateLine(g Gate) string {
	status := g.Status
	switch {
	case status == "" && g.Waived:
		status = "waived"
	case status == "":
		return GateOpen
	}
	parts := []string{status}
	if g.Version != "" {
		parts = append(parts, g.Version)
	}
	if g.By != "" {
		parts = append(parts, "by "+oneLine(g.By))
	}
	if !g.At.IsZero() {
		parts = append(parts, "on "+day(g.At))
	}
	if g.Waived && g.Status != "" {
		parts = append(parts, "(waived)")
	}
	return strings.Join(parts, " ")
}

// day formats a time as its UTC date, so a snapshot renders the same on
// every machine.
func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// oneLine keeps a record text on one markdown line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
