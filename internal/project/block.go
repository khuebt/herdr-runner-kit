package project

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Sections of the shared yaml block.
const (
	SectionMilestones    = "milestones"
	SectionDependencies  = "dependencies"
	SectionNodes         = "nodes"
	SectionOpenQuestions = "openQuestions"
	SectionBacklog       = "backlog"
)

// Consumer is a reader of the shared yaml block: the sections it accepts,
// and those it needs.
type Consumer struct {
	Name     string
	Accepts  []string
	Requires []string
}

// The readers of the shared yaml block.
var (
	RoadmapProposal = Consumer{
		Name:    "roadmap proposal",
		Accepts: []string{SectionMilestones, SectionDependencies}, Requires: []string{SectionMilestones},
	}
	MilestonePlan = Consumer{
		Name:    "milestone plan",
		Accepts: []string{SectionNodes, SectionOpenQuestions, SectionBacklog}, Requires: []string{SectionNodes},
	}
	RoadmapUpdate = Consumer{
		Name:    "roadmap update",
		Accepts: []string{SectionMilestones}, Requires: []string{SectionMilestones},
	}
)

// Block is the one schema of the fenced yaml block that roadmap proposals,
// milestone plans and roadmap updates end with.
type Block struct {
	Milestones    []BlockMilestone  `yaml:"milestones"`
	Dependencies  []BlockDependency `yaml:"dependencies"`
	Nodes         []BlockNode       `yaml:"nodes"`
	OpenQuestions []OpenQuestion    `yaml:"openQuestions"`
	Backlog       []BlockBacklog    `yaml:"backlog"`
}

// BlockMilestone is a milestone of a roadmap. Planned is a date,
// YYYY-MM-DD.
type BlockMilestone struct {
	ID       string    `yaml:"id"`
	Title    string    `yaml:"title"`
	Planned  string    `yaml:"planned"`
	Features []Feature `yaml:"features"`
}

// BlockDependency is something outside the project a milestone waits on.
// SecretLocation says where a secret is kept, never its value.
type BlockDependency struct {
	Name           string `yaml:"name"`
	Owner          string `yaml:"owner"`
	Milestone      string `yaml:"milestone"`
	Workaround     string `yaml:"workaround"`
	SecretLocation string `yaml:"secretLocation"`
}

// Node types of a milestone plan.
const (
	NodeFeature   = "feature"
	NodeIntegrate = "integrate"
)

// BlockNode is one runner of a milestone plan.
type BlockNode struct {
	ID        string   `yaml:"id"`
	Type      string   `yaml:"type"`
	Title     string   `yaml:"title"`
	DependsOn []string `yaml:"dependsOn"`
	Branch    string   `yaml:"branch"`
}

// OpenQuestion is a question of a milestone plan and the answer it took.
type OpenQuestion struct {
	Q      string `yaml:"q"`
	Answer string `yaml:"answer"`
}

// BlockBacklog moves a backlog item into a node of the plan.
type BlockBacklog struct {
	ID string `yaml:"id"`
	To string `yaml:"to"`
}

// openFence matches the opening line of a fenced code block and captures
// the fence and its info string.
var openFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})[ \t]*([^`]*)$")

// FindBlock returns the body of the first fenced code block tagged yaml in
// a markdown file.
func FindBlock(markdown []byte) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(markdown))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	fence, tagged := "", false
	var body bytes.Buffer
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if fence == "" {
			m := openFence.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			fence = m[1]
			info := strings.Fields(m[2])
			tagged = len(info) > 0 && strings.EqualFold(info[0], "yaml")
			continue
		}
		if closes(line, fence) {
			if tagged {
				return body.Bytes(), nil
			}
			fence = ""
			continue
		}
		if tagged {
			body.WriteString(line + "\n")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read markdown: %w", err)
	}
	if tagged {
		return nil, errors.New("the yaml block is not closed")
	}
	return nil, errors.New("no fenced block tagged yaml")
}

// closes reports whether line closes a block opened with fence.
func closes(line, fence string) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	run := len(trimmed) - len(strings.TrimLeft(trimmed, fence[:1]))
	return run >= len(fence) && strings.TrimSpace(trimmed[run:]) == ""
}

// ParseBlock reads the first yaml block of a markdown file for consumer c.
// Decoding is strict, every text field is flattened to one short line,
// sections c does not accept are refused, nodes may depend on features of
// done milestones, and the block is checked against rec, whose done
// milestones it may not change and whose done and running milestones it
// may not remove.
// Errors name the field at fault.
func ParseBlock(markdown []byte, c Consumer, rec Record) (Block, error) {
	raw, err := FindBlock(markdown)
	if err != nil {
		return Block{}, fmt.Errorf("%s: %w", c.Name, err)
	}
	var b Block
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&b); err != nil {
		if errors.Is(err, io.EOF) {
			return Block{}, fmt.Errorf("%s: the yaml block is empty", c.Name)
		}
		return Block{}, fmt.Errorf("%s: parse the yaml block: %w", c.Name, err)
	}
	b.flatten()
	b.dropDoneDeps(rec)
	present := map[string]bool{
		SectionMilestones: len(b.Milestones) > 0, SectionDependencies: len(b.Dependencies) > 0,
		SectionNodes: len(b.Nodes) > 0, SectionOpenQuestions: len(b.OpenQuestions) > 0, SectionBacklog: len(b.Backlog) > 0,
	}
	for _, s := range []string{SectionMilestones, SectionDependencies, SectionNodes, SectionOpenQuestions, SectionBacklog} {
		if present[s] && !slices.Contains(c.Accepts, s) {
			return Block{}, fmt.Errorf("%s: %s is not accepted here; want only %s", c.Name, s, strings.Join(c.Accepts, ", "))
		}
	}
	for _, s := range c.Requires {
		if !present[s] {
			return Block{}, fmt.Errorf("%s: %s is missing or empty", c.Name, s)
		}
	}
	if err := b.check(rec); err != nil {
		return Block{}, fmt.Errorf("%s: %w", c.Name, err)
	}
	return b, nil
}

// maxField bounds every text field of a block.
const maxField = maxNote

// flatten makes every text field of b one line of at most maxField runes,
// so no field can carry line breaks or long text into the record, the
// rendered files or a runner's preamble.
func (b *Block) flatten() {
	for i := range b.Milestones {
		m := &b.Milestones[i]
		flat(&m.ID, &m.Title, &m.Planned)
		for j := range m.Features {
			f := &m.Features[j]
			flat(&f.ID, &f.Title)
			flatAll(f.DependsOn)
		}
	}
	for i := range b.Dependencies {
		d := &b.Dependencies[i]
		flat(&d.Name, &d.Owner, &d.Milestone, &d.Workaround, &d.SecretLocation)
	}
	for i := range b.Nodes {
		n := &b.Nodes[i]
		flat(&n.ID, &n.Type, &n.Title, &n.Branch)
		flatAll(n.DependsOn)
	}
	for i := range b.OpenQuestions {
		flat(&b.OpenQuestions[i].Q, &b.OpenQuestions[i].Answer)
	}
	for i := range b.Backlog {
		flat(&b.Backlog[i].ID, &b.Backlog[i].To)
	}
}

// dropDoneDeps drops from the nodes' dependencies the features of rec's
// done milestones, which are merged already, so a node waits only for
// nodes of its block. An id that names a node of the block stays.
func (b *Block) dropDoneDeps(rec Record) {
	done := map[string]bool{}
	for _, m := range rec.Milestones {
		if m.Status == MilestoneDone {
			for _, f := range m.Features {
				done[f.ID] = true
			}
		}
	}
	for _, n := range b.Nodes {
		delete(done, n.ID)
	}
	for i := range b.Nodes {
		b.Nodes[i].DependsOn = slices.DeleteFunc(b.Nodes[i].DependsOn, func(d string) bool { return done[d] })
	}
}

func flat(fields ...*string) {
	for _, f := range fields {
		*f = clip(oneLine(*f), maxField)
	}
}

func flatAll(values []string) {
	for i := range values {
		flat(&values[i])
	}
}

func (b Block) check(rec Record) error {
	if err := b.checkMilestones(rec); err != nil {
		return err
	}
	milestones := make([]string, 0, len(b.Milestones))
	for _, m := range b.Milestones {
		milestones = append(milestones, m.ID)
	}
	names := map[string]bool{}
	for i, d := range b.Dependencies {
		field := fmt.Sprintf("dependencies[%d]", i)
		if strings.TrimSpace(d.Name) == "" {
			return fmt.Errorf("%s.name is empty", field)
		}
		if names[strings.ToLower(d.Name)] {
			return fmt.Errorf("%s.name %q is used twice", field, d.Name)
		}
		names[strings.ToLower(d.Name)] = true
		if d.Owner != "" && !slices.Contains(DependencyOwners, d.Owner) {
			return fmt.Errorf("%s.owner %q: want one of %s", field, d.Owner, strings.Join(DependencyOwners, ", "))
		}
		if d.Milestone != "" && !slices.Contains(milestones, d.Milestone) {
			return fmt.Errorf("%s.milestone %q is not a milestone of this block", field, d.Milestone)
		}
		if err := checkNoSecrets(map[string]string{
			field + ".name": d.Name, field + ".workaround": d.Workaround, field + ".secretLocation": d.SecretLocation,
		}); err != nil {
			return err
		}
	}
	nodes := map[string][]string{}
	var order []string
	for i, n := range b.Nodes {
		field := fmt.Sprintf("nodes[%d]", i)
		if err := checkID(field, n.ID, nodes); err != nil {
			return err
		}
		if n.Type != NodeFeature && n.Type != NodeIntegrate {
			return fmt.Errorf("%s.type %q: want %s or %s", field, n.Type, NodeFeature, NodeIntegrate)
		}
		if strings.TrimSpace(n.Title) == "" {
			return fmt.Errorf("%s.title is empty", field)
		}
		nodes[n.ID] = n.DependsOn
		order = append(order, n.ID)
	}
	if err := checkGraph("nodes", order, nodes); err != nil {
		return err
	}
	for i, q := range b.OpenQuestions {
		if strings.TrimSpace(q.Q) == "" {
			return fmt.Errorf("openQuestions[%d].q is empty", i)
		}
	}
	seen := map[string]bool{}
	for i, it := range b.Backlog {
		field := fmt.Sprintf("backlog[%d]", i)
		if strings.TrimSpace(it.ID) == "" {
			return fmt.Errorf("%s.id is empty", field)
		}
		if seen[it.ID] {
			return fmt.Errorf("%s.id %q is used twice", field, it.ID)
		}
		seen[it.ID] = true
		if _, ok := nodes[it.To]; !ok {
			return fmt.Errorf("%s.to %q is not a node of this block", field, it.To)
		}
	}
	return nil
}

// checkMilestones checks ids, dates and feature dependencies, that every
// done milestone of rec is in the block unchanged, and that the running
// one is still in it. The bare scaffold milestone, started before there
// was a roadmap, must be in the block but may change.
func (b Block) checkMilestones(rec Record) error {
	ids := map[string][]string{}
	features := map[string][]string{}
	var order []string
	for i, m := range b.Milestones {
		field := fmt.Sprintf("milestones[%d]", i)
		if err := checkID(field, m.ID, ids); err != nil {
			return err
		}
		ids[m.ID] = nil
		if strings.TrimSpace(m.Title) == "" {
			return fmt.Errorf("%s.title is empty", field)
		}
		if m.Planned != "" {
			if _, err := time.Parse(time.DateOnly, m.Planned); err != nil {
				return fmt.Errorf("%s.planned %q: want a date YYYY-MM-DD", field, m.Planned)
			}
		}
		for j, f := range m.Features {
			ff := fmt.Sprintf("%s.features[%d]", field, j)
			if err := checkID(ff, f.ID, features); err != nil {
				return err
			}
			if strings.TrimSpace(f.Title) == "" {
				return fmt.Errorf("%s.title is empty", ff)
			}
			features[f.ID] = f.DependsOn
			order = append(order, f.ID)
		}
	}
	if err := checkGraph("features", order, features); err != nil {
		return err
	}
	if len(b.Milestones) == 0 {
		return nil
	}
	for _, kept := range rec.Milestones {
		if kept.Status != MilestoneDone && kept.Status != MilestoneInProgress {
			continue
		}
		i := slices.IndexFunc(b.Milestones, func(m BlockMilestone) bool { return m.ID == kept.ID })
		if i < 0 {
			return fmt.Errorf("milestones: %s is %s and cannot be removed", kept.ID, kept.Status)
		}
		m := b.Milestones[i]
		if kept.Status == MilestoneDone && HasRoadmap(rec) && (m.Title != kept.Title || m.Planned != dateOrEmpty(kept.Planned) || !sameFeatures(m.Features, kept.Features)) {
			return fmt.Errorf("milestones[%d]: %s is done and cannot be changed", i, kept.ID)
		}
	}
	return nil
}

func checkID(field, id string, seen map[string][]string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%s.id is empty", field)
	}
	if strings.ContainsAny(id, " \t\n") {
		return fmt.Errorf("%s.id %q holds a space", field, id)
	}
	if _, dup := seen[id]; dup {
		return fmt.Errorf("%s.id %q is used twice", field, id)
	}
	return nil
}

// checkGraph refuses dependencies on unknown ids and cycles. order lists
// the ids as they appear, so errors are stable.
func checkGraph(section string, order []string, deps map[string][]string) error {
	for _, id := range order {
		for _, d := range deps[id] {
			if _, ok := deps[d]; !ok {
				return fmt.Errorf("%s: %s.dependsOn names %q, which is not in this block", section, id, d)
			}
			if d == id {
				return fmt.Errorf("%s: %s depends on itself", section, id)
			}
		}
	}
	const (
		unvisited = iota
		visiting
		visited
	)
	mark := map[string]int{}
	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch mark[id] {
		case visiting:
			return fmt.Errorf("%s: dependency cycle %s", section, strings.Join(append(path, id), " -> "))
		case visited:
			return nil
		}
		mark[id] = visiting
		for _, d := range deps[id] {
			if err := visit(d, append(path, id)); err != nil {
				return err
			}
		}
		mark[id] = visited
		return nil
	}
	for _, id := range order {
		if mark[id] == unvisited {
			if err := visit(id, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameFeatures(a, b []Feature) bool {
	return slices.EqualFunc(a, b, func(x, y Feature) bool {
		return x.ID == y.ID && x.Title == y.Title && slices.Equal(x.DependsOn, y.DependsOn)
	})
}

func dateOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return day(t)
}
