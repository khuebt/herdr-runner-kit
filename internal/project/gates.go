package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// Gate statuses. A gate with no outcome for its committed version is open.
const (
	GateOpen             = "open"
	GateApproved         = "approved"
	GateChangesRequested = "changes-requested"
	GateRejected         = "rejected"
)

// Milestone statuses the gates and the roadmap import use.
const (
	MilestonePlanned = "planned"
	MilestoneDone    = "done"
)

// Committed artifacts the gates read, relative to the repository root.
const (
	SpecPath            = DiscoveryDir + "/spec.md"
	RoadmapProposalPath = DiscoveryDir + "/roadmap-proposal.md"
	// PackHTML is the file every pack folder holds.
	PackHTML = "pack.html"
	PackPDF  = "pack.pdf"
)

// packDir matches the folder of one pack version.
var packDir = regexp.MustCompile(`^pack-v([1-9][0-9]{0,5})$`)

// PackDir returns the folder of pack version n, relative to the root.
func PackDir(n int) string { return fmt.Sprintf("%s/pack-v%d", ApprovalDir, n) }

// SpecInfo is what the gates read from the committed spec's front matter.
type SpecInfo struct {
	Version int
	Type    string
	Track   string
}

// CommittedSpec reads the spec as committed at HEAD. ok is false when HEAD
// holds no spec.
func CommittedSpec(ctx context.Context, root string) (SpecInfo, bool, error) {
	data, ok, err := committedFile(ctx, root, SpecPath)
	if err != nil || !ok {
		return SpecInfo{}, ok, err
	}
	front, _, err := splitFrontMatter(data)
	if err != nil {
		return SpecInfo{}, false, fmt.Errorf("committed %s %w", SpecPath, err)
	}
	var head struct {
		Version int    `yaml:"version"`
		Type    string `yaml:"type"`
		Track   string `yaml:"track"`
	}
	if err := yaml.Unmarshal(front, &head); err != nil {
		return SpecInfo{}, false, fmt.Errorf("committed %s: front matter: %w", SpecPath, err)
	}
	if head.Version < 1 {
		return SpecInfo{}, false, fmt.Errorf("committed %s: front matter needs version: a whole number from 1", SpecPath)
	}
	if head.Type != "" && !slices.Contains(Types, head.Type) {
		return SpecInfo{}, false, fmt.Errorf("committed %s: type %q: want one of %v", SpecPath, head.Type, Types)
	}
	if head.Track != "" && !slices.Contains(Tracks, head.Track) {
		return SpecInfo{}, false, fmt.Errorf("committed %s: track %q: want one of %v", SpecPath, head.Track, Tracks)
	}
	return SpecInfo{Version: head.Version, Type: head.Type, Track: head.Track}, true, nil
}

// CommittedPackVersion returns the highest pack version committed at HEAD
// under ApprovalDir, or 0 when there is none.
func CommittedPackVersion(ctx context.Context, root string) (int, error) {
	out, err := git(ctx, root, "ls-tree", "-d", "--name-only", "HEAD", "--", ApprovalDir+"/")
	if err != nil {
		return 0, err
	}
	highest := 0
	for line := range strings.Lines(out) {
		if m := packDir.FindStringSubmatch(path.Base(strings.TrimSpace(line))); m != nil {
			n, _ := strconv.Atoi(m[1]) // the pattern holds at most six digits
			highest = max(highest, n)
		}
	}
	return highest, nil
}

// committedFile returns the content of rel at HEAD. ok is false when HEAD
// does not hold it.
func committedFile(ctx context.Context, root, rel string) ([]byte, bool, error) {
	out, err := git(ctx, root, "ls-tree", "--name-only", "HEAD", "--", rel)
	if err != nil {
		return nil, false, err
	}
	if out != rel {
		return nil, false, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", root, "show", "HEAD:"+rel)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, false, fmt.Errorf("git show %s: %s", rel, firstLine(stderr.String(), err))
	}
	return data, true, nil
}

// uncommitted reports whether rel, a file or folder, differs from HEAD in
// the index or the working tree, untracked files included.
func uncommitted(ctx context.Context, root, rel string) (bool, error) {
	out, err := git(ctx, root, "status", "--porcelain", "--untracked-files=all", "--", rel)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Version formats a gate version.
func Version(n int) string { return "v" + strconv.Itoa(n) }

// versionNumber reads a gate version, 0 when there is none.
func versionNumber(v string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "v"))
	if err != nil {
		return 0
	}
	return n
}

// GateState is a gate as the committed artifacts and the record show it.
type GateState struct {
	Name string
	// Committed is the artifact's version at HEAD, 0 when none.
	Committed int
	// Track is the track of the committed spec.
	Track string
	// Outcome is the last outcome recorded for the gate.
	Outcome Gate
}

// ReadGate reads gate name of rec's project in root. Openness is derived
// from the artifacts committed at HEAD, so a record alone never opens a
// gate.
func ReadGate(ctx context.Context, root string, rec Record, name string) (GateState, error) {
	g := GateState{Name: name}
	spec, _, err := CommittedSpec(ctx, root)
	if err != nil {
		return g, err
	}
	g.Track = spec.Track
	switch name {
	case GateSpec:
		g.Committed, g.Outcome = spec.Version, rec.Gates.Spec
	case GatePack:
		g.Outcome = rec.Gates.Pack
		if g.Committed, err = CommittedPackVersion(ctx, root); err != nil {
			return g, err
		}
	default:
		return g, fmt.Errorf("unknown gate %q: want %s or %s", name, GateSpec, GatePack)
	}
	return g, nil
}

// Status is the gate's status for its committed version: the recorded
// outcome when it is for that version, else open. An approval stays in
// force while a later version waits for its own outcome.
func (g GateState) Status() string {
	if g.Outcome.Status != "" && versionNumber(g.Outcome.Version) == g.Committed {
		return g.Outcome.Status
	}
	if g.Passed() {
		return GateApproved
	}
	return GateOpen
}

// Passed reports whether work behind the gate may start: an approved
// version is committed at HEAD, or, for the pack only, the user waived it
// at spec approval and the committed spec is still on the lite track.
func (g GateState) Passed() bool {
	if g.Outcome.Status == GateApproved {
		if n := versionNumber(g.Outcome.Version); n > 0 && n <= g.Committed {
			return true
		}
	}
	return g.Name == GatePack && g.Outcome.Waived && g.Track == "lite"
}

// RequireGate refuses work behind gate name until it has passed.
func RequireGate(ctx context.Context, root string, rec Record, name string) error {
	g, err := ReadGate(ctx, root, rec, name)
	if err != nil {
		return err
	}
	if g.Passed() {
		return nil
	}
	if o := g.Outcome; o.Status == GateApproved && versionNumber(o.Version) > g.Committed {
		return fmt.Errorf("the record says the %s gate approved %s, but HEAD of %s holds %s; commit the approved version or record a new outcome",
			name, o.Version, root, committedLabel(name, g.Committed))
	}
	if g.Outcome.Waived && g.Track != "lite" {
		return fmt.Errorf("the pack gate was waived for the lite track, but the committed spec is on the %s track; approve a pack (herdr-runner-kit project approve pack)", orDash(g.Track))
	}
	return fmt.Errorf("the %s gate is %s; record its approval first with: herdr-runner-kit project approve %s", name, g.Status(), name)
}

func committedLabel(gate string, n int) string {
	switch {
	case n > 0:
		return Version(n)
	case gate == GatePack:
		return "no pack"
	}
	return "no spec"
}

// Outcomes the user records at a gate, by their command.
var outcomeOf = map[string]string{
	"approve": GateApproved,
	"changes": GateChangesRequested,
	"reject":  GateRejected,
}

// DecideOptions is one gate outcome the user records.
type DecideOptions struct {
	Gate string
	// Outcome is approve, changes or reject.
	Outcome string
	// By names the approver; Note is optional except on a rejection.
	By, Note string
	// WaivePack skips the pack gate on the lite track; only with
	// approve spec.
	WaivePack bool
	Now       time.Time
}

// Decided is a recorded and committed gate outcome.
type Decided struct {
	Gate, Status, Version string
	// Waived is set when the spec approval waived the pack gate.
	Waived bool
	// Milestones and Dependencies count what a pack approval imported
	// from the roadmap proposal.
	Milestones, Dependencies int
	// NoPDF is set when the approved pack holds no PDF.
	NoPDF bool
	// Commit is the commit that holds the records.
	Commit string
}

// Decide records a gate outcome for the committed version of its artifact
// and commits every pending record in one commit. It refuses an artifact
// that is not committed or has uncommitted changes, and a version that
// already has an outcome. Approving the pack imports the roadmap block of
// the committed roadmap proposal and clears the re-approval flag.
func Decide(ctx context.Context, s *state.Store, name string, o DecideOptions) (Decided, error) {
	status, ok := outcomeOf[o.Outcome]
	if !ok {
		return Decided{}, fmt.Errorf("unknown outcome %q: want approve, changes or reject", o.Outcome)
	}
	if o.Gate != GateSpec && o.Gate != GatePack {
		return Decided{}, fmt.Errorf("unknown gate %q: want %s or %s", o.Gate, GateSpec, GatePack)
	}
	if o.WaivePack && (o.Gate != GateSpec || status != GateApproved) {
		return Decided{}, errors.New("--waive-pack goes only with approve spec")
	}
	o.By, o.Note = oneLine(o.By), clip(oneLine(o.Note), maxNote)
	if o.By == "" {
		return Decided{}, errors.New("name who decided with --by")
	}
	if status == GateRejected && o.Note == "" {
		return Decided{}, errors.New("say why with --note")
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	p, err := s.ProjectByName(name)
	if err != nil {
		return Decided{}, err
	}
	if p.Reserved {
		return Decided{}, fmt.Errorf("project %s is still being set up", name)
	}
	rec, err := Load(s, name)
	if err != nil {
		return Decided{}, err
	}
	if err := checkOrder(ctx, p.Path, rec, o.Gate); err != nil {
		return Decided{}, err
	}
	spec, n, err := committedArtifact(ctx, p.Path, o.Gate)
	if err != nil {
		return Decided{}, err
	}
	d := Decided{Gate: o.Gate, Status: status, Version: Version(n), Waived: o.WaivePack}
	if err := checkNew(rec, o.Gate, n); err != nil {
		return Decided{}, err
	}
	if o.WaivePack && spec.Track != "lite" {
		return Decided{}, fmt.Errorf("only a lite-track project may skip the pack gate, and the committed spec says track %s", orDash(spec.Track))
	}
	var roadmap []byte
	if o.Gate == GatePack {
		if d.NoPDF, err = missing(ctx, p.Path, PackDir(n)+"/"+PackPDF); err != nil {
			return Decided{}, err
		}
		if status == GateApproved {
			var ok bool
			if roadmap, ok, err = committedFile(ctx, p.Path, RoadmapProposalPath); err != nil {
				return Decided{}, err
			} else if !ok {
				return Decided{}, fmt.Errorf("approving the pack imports the roadmap, and HEAD holds no %s", RoadmapProposalPath)
			}
		}
	}
	// Refuse early while the checkout is busy, so the outcome is not left
	// recorded but uncommitted; the commit checks again under its lock.
	if err := checkCheckout(ctx, s, p); err != nil {
		return Decided{}, err
	}
	if _, err := Update(s, name, func(rec *Record) error {
		if err := checkOrder(ctx, p.Path, *rec, o.Gate); err != nil {
			return err
		}
		if err := checkNew(*rec, o.Gate, n); err != nil {
			return err
		}
		gate := &rec.Gates.Spec
		if o.Gate == GatePack {
			gate = &rec.Gates.Pack
		}
		*gate = Gate{Status: status, Version: d.Version, By: o.By, At: o.Now.UTC(), Note: o.Note, Waived: gate.Waived}
		text := fmt.Sprintf("%s %s %s by %s", o.Gate, d.Version, strings.ReplaceAll(status, "-", " "), o.By)
		if o.Gate == GateSpec && status == GateApproved {
			if spec.Type != "" {
				rec.Type = spec.Type
			}
			if spec.Track != "" {
				rec.Track = spec.Track
			}
		}
		if o.WaivePack {
			rec.Gates.Pack = Gate{Waived: true, By: o.By, At: o.Now.UTC(), Note: "lite track: pack waived at spec " + d.Version + " approval"}
			text += "; the pack gate is waived for the lite track by the user's choice"
		}
		if o.Gate == GatePack && status == GateApproved {
			b, err := ParseBlock(roadmap, RoadmapProposal, *rec)
			if err != nil {
				return fmt.Errorf("%s: %w", RoadmapProposalPath, err)
			}
			d.Milestones, d.Dependencies = applyRoadmap(rec, b)
			rec.Reapproval = Reapproval{}
			gate.Waived = false
		}
		if o.Note != "" {
			text += ": " + o.Note
		}
		rec.Decisions = append(rec.Decisions, Decision{At: o.Now.UTC(), Text: text})
		return nil
	}); err != nil {
		return Decided{}, err
	}
	sha, err := Commit(ctx, s, name, fmt.Sprintf("docs(project): %s %s %s", commitVerb[status], o.Gate, d.Version))
	if err != nil {
		return d, fmt.Errorf("%s %s is %s in the kit's record, but its records are not committed: %w", o.Gate, d.Version, status, err)
	}
	d.Commit = sha
	return d, nil
}

// checkOrder refuses a pack outcome before the spec has passed.
func checkOrder(ctx context.Context, root string, rec Record, gate string) error {
	if gate != GatePack {
		return nil
	}
	if err := RequireGate(ctx, root, rec, GateSpec); err != nil {
		return fmt.Errorf("the pack is decided after the spec: %w", err)
	}
	return nil
}

// checkNew refuses an outcome for a version that already has one, or for
// an older version.
func checkNew(rec Record, gate string, n int) error {
	g := rec.Gates.Spec
	if gate == GatePack {
		g = rec.Gates.Pack
	}
	if g.Status != "" && n <= versionNumber(g.Version) {
		return fmt.Errorf("%s %s already has an outcome (%s %s); the runner revises it as a new version first",
			gate, Version(n), g.Status, g.Version)
	}
	return nil
}

// maxNote bounds the note an outcome keeps in the record; the full
// feedback goes to the runner.
const maxNote = 300

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

var commitVerb = map[string]string{
	GateApproved:         "approve",
	GateChangesRequested: "request changes to",
	GateRejected:         "reject",
}

// committedArtifact returns the version of gate's artifact committed at
// HEAD, refusing one that is missing or has uncommitted changes. It also
// returns the committed spec, which every gate reads.
func committedArtifact(ctx context.Context, root, gate string) (SpecInfo, int, error) {
	spec, ok, err := CommittedSpec(ctx, root)
	if err != nil {
		return SpecInfo{}, 0, err
	}
	if gate == GateSpec {
		if !ok {
			return SpecInfo{}, 0, fmt.Errorf("HEAD holds no %s; the spec runner commits it before it reports done", SpecPath)
		}
		dirty, err := uncommitted(ctx, root, SpecPath)
		if err != nil {
			return SpecInfo{}, 0, err
		}
		if dirty {
			return SpecInfo{}, 0, fmt.Errorf("%s has changes not committed; decide on a committed version", SpecPath)
		}
		return spec, spec.Version, nil
	}
	n, err := CommittedPackVersion(ctx, root)
	if err != nil {
		return SpecInfo{}, 0, err
	}
	if n == 0 {
		return SpecInfo{}, 0, fmt.Errorf("HEAD holds no pack under %s/; run the pack stage first", ApprovalDir)
	}
	if gone, err := missing(ctx, root, PackDir(n)+"/"+PackHTML); err != nil {
		return SpecInfo{}, 0, err
	} else if gone {
		return SpecInfo{}, 0, fmt.Errorf("HEAD holds no %s/%s", PackDir(n), PackHTML)
	}
	dirty, err := uncommitted(ctx, root, PackDir(n))
	if err != nil {
		return SpecInfo{}, 0, err
	}
	if dirty {
		return SpecInfo{}, 0, fmt.Errorf("%s has changes not committed; decide on a committed version", PackDir(n))
	}
	return spec, n, nil
}

// missing reports whether HEAD lacks the file rel.
func missing(ctx context.Context, root, rel string) (bool, error) {
	out, err := git(ctx, root, "ls-tree", "--name-only", "HEAD", "--", rel)
	return out != rel, err
}

// applyRoadmap replaces the record's milestones with the block's, keeping
// what the kit recorded about each one, and adds or updates dependencies
// by name. It returns how many of each the block held.
func applyRoadmap(rec *Record, b Block) (milestones, dependencies int) {
	old := map[string]Milestone{}
	for _, m := range rec.Milestones {
		old[m.ID] = m
	}
	next := make([]Milestone, 0, len(b.Milestones))
	for _, bm := range b.Milestones {
		m := old[bm.ID]
		if m.Status != MilestoneDone {
			m.ID, m.Title, m.Features = bm.ID, bm.Title, bm.Features
			m.Planned = time.Time{}
			if bm.Planned != "" {
				m.Planned, _ = time.Parse(time.DateOnly, bm.Planned) // checked by ParseBlock
			}
			if m.Status == "" {
				m.Status = MilestonePlanned
			}
		}
		next = append(next, m)
	}
	rec.Milestones = next
	for _, bd := range b.Dependencies {
		i := slices.IndexFunc(rec.Dependencies, func(d Dependency) bool { return d.Name == bd.Name })
		if i < 0 {
			rec.Dependencies = append(rec.Dependencies, Dependency{ID: nextID("D", depIDs(rec.Dependencies)), Name: bd.Name, Status: "needed"})
			i = len(rec.Dependencies) - 1
		}
		d := &rec.Dependencies[i]
		d.Owner, d.Milestone, d.Workaround, d.SecretLocation = bd.Owner, bd.Milestone, bd.Workaround, bd.SecretLocation
	}
	return len(b.Milestones), len(b.Dependencies)
}

func depIDs(deps []Dependency) []string {
	ids := make([]string, 0, len(deps))
	for _, d := range deps {
		ids = append(ids, d.ID)
	}
	return ids
}

// nextID returns prefix followed by one more than the highest number of
// the ids that carry prefix.
func nextID(prefix string, ids []string) string {
	highest := 0
	for _, id := range ids {
		if n, err := strconv.Atoi(strings.TrimPrefix(id, prefix)); err == nil && strings.HasPrefix(id, prefix) {
			highest = max(highest, n)
		}
	}
	return prefix + strconv.Itoa(highest+1)
}

// Approver returns the name git uses as author in root, or "" when none
// is set.
func Approver(ctx context.Context, root string) string {
	out, err := git(ctx, root, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(out, " <")
	return strings.TrimSpace(name)
}

// AddDecision keeps a decision of the user in the record. It is committed
// with the next gate outcome or milestone.
func AddDecision(s *state.Store, name, text string, now time.Time) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the decision is empty")
	}
	_, err := Update(s, name, func(rec *Record) error {
		rec.Decisions = append(rec.Decisions, Decision{At: now.UTC(), Text: text})
		return nil
	})
	return err
}
