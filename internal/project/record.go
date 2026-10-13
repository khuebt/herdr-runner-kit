// Package project keeps the record of a project the kit drives: its typed
// record in kit state, the snapshot rendered into its repository, and the
// batched commit that writes that snapshot.
package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// ManifestVersion is the record schema this binary writes; it reads every
// version up to it. Version 2 added the gate's content id and the change
// request's file, which a version 1 reader refuses as unknown fields, so
// that reader is told to upgrade instead.
const ManifestVersion = 2

// Record defaults.
const (
	DefaultTagFormat       = "v0.{N}.0"
	DefaultParallelWarning = 3
)

// Project types and tracks.
var (
	Types  = []string{"ui", "cli", "api", "library", "unknown"}
	Tracks = []string{"lite", "standard"}
)

// ErrNoRecord is returned when the kit holds no record for a project.
var ErrNoRecord = errors.New("no project record")

// Record is everything the kit knows about one project. The kit's copy in
// state is the source of truth between commits; the repository holds the
// snapshot rendered at each commit point.
type Record struct {
	ManifestVersion int           `json:"manifestVersion" yaml:"manifestVersion"`
	Name            string        `json:"name" yaml:"name"`
	Type            string        `json:"type" yaml:"type"`
	Track           string        `json:"track" yaml:"track"`
	ReferenceRepo   string        `json:"referenceRepo,omitempty" yaml:"referenceRepo,omitempty"`
	TagFormat       string        `json:"tagFormat" yaml:"tagFormat"`
	ParallelWarning int           `json:"parallelWarning" yaml:"parallelWarning"`
	Share           Share         `json:"share" yaml:"share"`
	Gates           Gates         `json:"gates" yaml:"gates"`
	Reapproval      Reapproval    `json:"reapproval" yaml:"reapproval"`
	Milestones      []Milestone   `json:"milestones,omitempty" yaml:"milestones,omitempty"`
	Changes         []Change      `json:"changes,omitempty" yaml:"changes,omitempty"`
	Backlog         []BacklogItem `json:"backlog,omitempty" yaml:"backlog,omitempty"`
	Dependencies    []Dependency  `json:"dependencies,omitempty" yaml:"dependencies,omitempty"`
	Decisions       []Decision    `json:"decisions,omitempty" yaml:"decisions,omitempty"`
	Graduated       *Graduated    `json:"graduated,omitempty" yaml:"graduated,omitempty"`
	// Pending counts the changes made since the snapshot was last
	// committed. It lives only in kit state.
	Pending int `json:"pending,omitempty" yaml:"-"`
}

// Share holds the user's opt-ins to share work outside the machine.
type Share struct {
	Stitch       bool `json:"stitch" yaml:"stitch"`
	LavishHosted bool `json:"lavishHosted" yaml:"lavishHosted"`
}

// Gates are the two approvals a project passes.
type Gates struct {
	Spec Gate `json:"spec" yaml:"spec"`
	Pack Gate `json:"pack" yaml:"pack"`
}

// Gate is the outcome of one approval.
type Gate struct {
	Status  string    `json:"status,omitempty" yaml:"status,omitempty"`
	Version string    `json:"version,omitempty" yaml:"version,omitempty"`
	By      string    `json:"by,omitempty" yaml:"by,omitempty"`
	At      time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	Note    string    `json:"note,omitempty" yaml:"note,omitempty"`
	Waived  bool      `json:"waived,omitempty" yaml:"waived,omitempty"`
	// Content is the git object id of the decided version: the blob of
	// the spec file or the tree of the pack folder. The outcome holds
	// only while that version's committed content is unchanged.
	Content string `json:"content,omitempty" yaml:"content,omitempty"`
}

// Reapproval is set when a change of scope needs the pack approved again.
type Reapproval struct {
	Needed bool   `json:"needed" yaml:"needed"`
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// Milestone is one release-sized step of the roadmap.
type Milestone struct {
	ID       string    `json:"id" yaml:"id"`
	Title    string    `json:"title" yaml:"title"`
	Status   string    `json:"status,omitempty" yaml:"status,omitempty"`
	Planned  time.Time `json:"planned,omitzero" yaml:"planned,omitempty"`
	Started  time.Time `json:"started,omitzero" yaml:"started,omitempty"`
	Closed   time.Time `json:"closed,omitzero" yaml:"closed,omitempty"`
	Tag      string    `json:"tag,omitempty" yaml:"tag,omitempty"`
	Branch   string    `json:"branch,omitempty" yaml:"branch,omitempty"`
	Features []Feature `json:"features,omitempty" yaml:"features,omitempty"`
	// Released holds the git blob id of the release notes each done of
	// the milestone found merged, oldest first, so the release merged
	// before a reopen is never taken for a new one.
	Released []string `json:"released,omitempty" yaml:"released,omitempty"`
	// RoadmapUpdate is the git blob id of the roadmap update the last
	// done found, so a later done imports only a new one.
	RoadmapUpdate string `json:"roadmapUpdate,omitempty" yaml:"roadmapUpdate,omitempty"`
}

// Feature is one unit of a milestone.
type Feature struct {
	ID        string   `json:"id" yaml:"id"`
	Title     string   `json:"title" yaml:"title"`
	DependsOn []string `json:"dependsOn,omitempty" yaml:"dependsOn,omitempty"`
}

// Change is a change request against the approved scope.
type Change struct {
	ID        string    `json:"id" yaml:"id"`
	Title     string    `json:"title" yaml:"title"`
	Class     string    `json:"class,omitempty" yaml:"class,omitempty"`
	Status    string    `json:"status,omitempty" yaml:"status,omitempty"`
	Blocking  bool      `json:"blocking,omitempty" yaml:"blocking,omitempty"`
	Milestone string    `json:"milestone,omitempty" yaml:"milestone,omitempty"`
	At        time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	// Path is the CR file, relative to the repository root.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// Task is the impact runner that writes the file. Task ids are local
	// to this machine, so it lives only in kit state.
	Task string `json:"task,omitempty" yaml:"-"`
}

// BacklogItem is an idea kept for later.
type BacklogItem struct {
	ID     string    `json:"id" yaml:"id"`
	Text   string    `json:"text" yaml:"text"`
	At     time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	Status string    `json:"status,omitempty" yaml:"status,omitempty"`
	To     string    `json:"to,omitempty" yaml:"to,omitempty"`
}

// Dependency is something outside the project the work waits on. Secrets
// are never stored: SecretLocation only says where one is kept.
type Dependency struct {
	ID             string `json:"id" yaml:"id"`
	Name           string `json:"name" yaml:"name"`
	Owner          string `json:"owner,omitempty" yaml:"owner,omitempty"`
	Status         string `json:"status,omitempty" yaml:"status,omitempty"`
	Milestone      string `json:"milestone,omitempty" yaml:"milestone,omitempty"`
	Workaround     string `json:"workaround,omitempty" yaml:"workaround,omitempty"`
	SecretLocation string `json:"secretLocation,omitempty" yaml:"secretLocation,omitempty"`
}

// Decision is one dated entry of the decision log.
type Decision struct {
	At   time.Time `json:"at" yaml:"at"`
	Text string    `json:"text" yaml:"text"`
}

// Graduated is set once the project left the kit's lifecycle.
type Graduated struct {
	At     time.Time `json:"at" yaml:"at"`
	Tag    string    `json:"tag,omitempty" yaml:"tag,omitempty"`
	Export string    `json:"export,omitempty" yaml:"export,omitempty"`
}

// NewRecord returns a fresh record for a project of unknown type on the
// standard track.
func NewRecord(name string) Record {
	return Record{
		ManifestVersion: ManifestVersion, Name: name, Type: "unknown", Track: "standard",
		TagFormat: DefaultTagFormat, ParallelWarning: DefaultParallelWarning,
	}
}

// Validate checks the fields every record must hold.
func (r *Record) Validate() error {
	if err := checkVersion(r.ManifestVersion); err != nil {
		return err
	}
	if err := state.CheckProjectName(r.Name); err != nil {
		return err
	}
	if !slices.Contains(Types, r.Type) {
		return fmt.Errorf("project type %q: want one of %v", r.Type, Types)
	}
	if !slices.Contains(Tracks, r.Track) {
		return fmt.Errorf("project track %q: want one of %v", r.Track, Tracks)
	}
	if r.ParallelWarning < 0 {
		return fmt.Errorf("parallelWarning %d is negative", r.ParallelWarning)
	}
	for name, g := range map[string]Gate{GateSpec: r.Gates.Spec, GatePack: r.Gates.Pack} {
		if g.Status != "" && !slices.Contains([]string{GateApproved, GateChangesRequested, GateRejected}, g.Status) {
			return fmt.Errorf("%s gate status %q: want %s, %s or %s", name, g.Status, GateApproved, GateChangesRequested, GateRejected)
		}
	}
	if r.Gates.Spec.Waived {
		return errors.New("the spec gate cannot be waived")
	}
	return nil
}

func checkVersion(v int) error {
	switch {
	case v == 0:
		return errors.New("project record has no manifestVersion")
	case v > ManifestVersion:
		return fmt.Errorf("project record uses manifestVersion %d, newer than this binary supports (%d): upgrade herdr-runner-kit",
			v, ManifestVersion)
	case v < 0:
		return fmt.Errorf("invalid manifestVersion %d", v)
	}
	return nil
}

// ParseSnapshot reads the record from the front matter of a rendered
// project.md. Unknown fields are refused, and a newer manifest is refused
// before its fields are looked at.
func ParseSnapshot(data []byte) (Record, error) {
	front, err := frontMatter(data)
	if err != nil {
		return Record{}, err
	}
	var head struct {
		ManifestVersion int `yaml:"manifestVersion"`
	}
	if err := yaml.Unmarshal(front, &head); err != nil {
		return Record{}, fmt.Errorf("parse project snapshot: %w", err)
	}
	if err := checkVersion(head.ManifestVersion); err != nil {
		return Record{}, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(front))
	dec.KnownFields(true)
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("parse project snapshot: %w", err)
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// frontMatter returns the YAML between the opening "---" line and the next
// "---" line of a project snapshot.
func frontMatter(data []byte) ([]byte, error) {
	front, _, err := splitFrontMatter(data)
	if err != nil {
		return nil, fmt.Errorf("project snapshot %w", err)
	}
	return front, nil
}

// splitFrontMatter returns the YAML between the opening "---" line and the
// next "---" line, and the text after it.
func splitFrontMatter(data []byte) (front, body []byte, err error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return nil, nil, errors.New("has no front matter")
	}
	if bytes.HasPrefix(rest, []byte("---\n")) {
		return nil, nil, errors.New("has empty front matter")
	}
	front, body, ok = bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return nil, nil, errors.New("front matter is not closed")
	}
	return append(front, '\n'), body, nil
}

func recordPath(s *state.Store, name string) string {
	return filepath.Join(s.ProjectDir(name), "record.json")
}

// Load reads the kit's record of a project.
func Load(s *state.Store, name string) (Record, error) {
	if err := state.CheckProjectName(name); err != nil {
		return Record{}, err
	}
	data, err := os.ReadFile(recordPath(s, name))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("%s: %w", name, ErrNoRecord)
	}
	if err != nil {
		return Record{}, fmt.Errorf("read project record: %w", err)
	}
	var head struct {
		ManifestVersion int `json:"manifestVersion"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return Record{}, fmt.Errorf("parse project record %s: %w", name, err)
	}
	if err := checkVersion(head.ManifestVersion); err != nil {
		return Record{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, fmt.Errorf("parse project record %s: %w", name, err)
	}
	return rec, nil
}

// Save writes the kit's record of a project, replacing any earlier one.
func Save(s *state.Store, rec Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	unlock, err := s.LockProject(rec.Name, "record")
	if err != nil {
		return err
	}
	defer unlock()
	return write(s, rec)
}

// Update applies fn to the kit's record under its lock and saves it as one
// more change not yet committed. If fn returns an error, nothing is written.
func Update(s *state.Store, name string, fn func(*Record) error) (Record, error) {
	return update(s, name, func(rec *Record) error {
		if err := fn(rec); err != nil {
			return err
		}
		rec.Pending++
		return nil
	})
}

func update(s *state.Store, name string, fn func(*Record) error) (Record, error) {
	unlock, err := s.LockProject(name, "record")
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	rec, err := Load(s, name)
	if err != nil {
		return Record{}, err
	}
	if err := fn(&rec); err != nil {
		return Record{}, err
	}
	if rec.Name != name {
		return Record{}, fmt.Errorf("project record %s cannot be renamed to %s", name, rec.Name)
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, write(s, rec)
}

func write(s *state.Store, rec Record) error {
	rec.ManifestVersion = ManifestVersion
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project record: %w", err)
	}
	if err := fsutil.WriteFileAtomic(recordPath(s, rec.Name), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write project record: %w", err)
	}
	return nil
}
