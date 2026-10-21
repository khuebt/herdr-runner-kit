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

	"github.com/khuebt/herdr-runner-kit/internal/redact"
	"github.com/khuebt/herdr-runner-kit/internal/state"
)

// Gate statuses. A gate with no outcome for its committed version is open.
const (
	GateOpen             = "open"
	GateApproved         = "approved"
	GateChangesRequested = "changes-requested"
	GateRejected         = "rejected"
	// GateChanged is the status of an approved version whose committed
	// content is no longer the content that was approved. It is derived,
	// never recorded.
	GateChanged = "changed-since-approval"
)

// Milestone statuses. A milestone in progress is the running milestone,
// whose scope change control protects.
const (
	MilestonePlanned    = "planned"
	MilestoneInProgress = "in-progress"
	MilestoneDone       = "done"
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

// PackDir returns the folder of pack version n before graduation,
// relative to the root.
func PackDir(n int) string { return packIn(ApprovalDir, n) }

// PackFolder returns the folder of pack version n of rec's project, which
// moves out of the discovery folder at graduation.
func PackFolder(rec Record, n int) string { return packIn(ApprovalFolder(rec), n) }

func packIn(dir string, n int) string { return fmt.Sprintf("%s/pack-v%d", dir, n) }

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

// CommittedPackVersion returns the highest pack version of rec's project
// committed at HEAD, or 0 when there is none.
func CommittedPackVersion(ctx context.Context, root string, rec Record) (int, error) {
	out, err := git(ctx, root, "ls-tree", "-d", "--name-only", "HEAD", "--", ApprovalFolder(rec)+"/")
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
	// Changed is set when the content committed for the outcome's version
	// is not the content the outcome was recorded for.
	Changed bool
}

// ReadGate reads gate name of rec's project in root. Openness is derived
// from the artifacts committed at HEAD, so a record alone never opens a
// gate. Once the project graduated, its spec is archived and the spec
// gate stays as it was decided, while the pack gate reads the packs kept
// outside the discovery folder.
func ReadGate(ctx context.Context, root string, rec Record, name string) (GateState, error) {
	g := GateState{Name: name}
	var spec SpecInfo
	if rec.Graduated != nil {
		spec = SpecInfo{Version: versionNumber(rec.Gates.Spec.Version), Track: rec.Track}
	} else {
		var err error
		if spec, _, err = CommittedSpec(ctx, root); err != nil {
			return g, err
		}
	}
	g.Track = spec.Track
	switch name {
	case GateSpec:
		g.Committed, g.Outcome = spec.Version, rec.Gates.Spec
		if rec.Graduated != nil {
			return g, nil
		}
	case GatePack:
		g.Outcome = rec.Gates.Pack
		var err error
		if g.Committed, err = CommittedPackVersion(ctx, root, rec); err != nil {
			return g, err
		}
	default:
		return g, fmt.Errorf("unknown gate %q: want %s or %s", name, GateSpec, GatePack)
	}
	// The spec file holds only its latest version, so an older version's
	// content cannot be checked; each pack version keeps its own folder.
	if o := g.Outcome; o.Content != "" && (name == GatePack || versionNumber(o.Version) == g.Committed) {
		id, err := committedID(ctx, root, artifactPath(rec, name, versionNumber(o.Version)))
		if err != nil {
			return g, err
		}
		g.Changed = id != "" && id != o.Content
	}
	return g, nil
}

// artifactPath returns the committed artifact of version n of gate.
func artifactPath(rec Record, gate string, n int) string {
	if gate == GatePack {
		return PackFolder(rec, n)
	}
	return SpecPath
}

// committedID returns the git object id of rel at HEAD, the blob of a file
// or the tree of a folder, or "" when HEAD does not hold rel.
func committedID(ctx context.Context, root, rel string) (string, error) {
	out, err := git(ctx, root, "ls-tree", "HEAD", "--", rel)
	if err != nil {
		return "", err
	}
	meta, entry, ok := strings.Cut(out, "\t")
	if !ok || entry != rel {
		return "", nil
	}
	f := strings.Fields(meta)
	if len(f) != 3 {
		return "", fmt.Errorf("git ls-tree %s: unexpected output %q", rel, out)
	}
	return f[2], nil
}

// Status is the gate's status for its committed version: the recorded
// outcome when it is for that version and its content, else open. An
// approval stays in force while a later version waits for its own outcome;
// an approved version whose content changed is GateChanged.
func (g GateState) Status() string {
	if g.Outcome.Status != "" && versionNumber(g.Outcome.Version) == g.Committed {
		switch {
		case !g.Changed:
			return g.Outcome.Status
		case g.Outcome.Status == GateApproved:
			return GateChanged
		}
		return GateOpen
	}
	if g.Passed() {
		return GateApproved
	}
	return GateOpen
}

// Passed reports whether work behind the gate may start: an approved
// version is committed at HEAD with the content that was approved, or, for the pack only, the user waived it
// at spec approval and the committed spec is still on the lite track.
func (g GateState) Passed() bool {
	if g.Outcome.Status == GateApproved && !g.Changed {
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
	if o := g.Outcome; o.Status == GateApproved && g.Changed {
		return fmt.Errorf("%s %s changed since its approval: HEAD of %s holds other content under the same version; ask the user to approve it again (hrk project approve %s)",
			name, o.Version, root, name)
	}
	if g.Outcome.Waived && g.Track != "lite" {
		return fmt.Errorf("the pack gate was waived for the lite track, but the committed spec is on the %s track; approve a pack (hrk project approve pack)", orDash(g.Track))
	}
	return fmt.Errorf("the %s gate is %s; record its approval first with: hrk project approve %s", name, g.Status(), name)
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
	// NoPDF is set when the approved pack holds no PDF; Folder is the
	// decided pack's folder.
	NoPDF  bool
	Folder string
	// Roadmap is set when a pack approval imported the roadmap proposal,
	// which a graduated project no longer has.
	Roadmap bool
	// Replaced is the earlier outcome of the same version, which this one
	// replaces because the version's content changed since; "" when none.
	Replaced string
	// Commit is the commit that holds the records.
	Commit string
}

// Decide records a gate outcome for the committed version of its artifact,
// bound to that version's committed content, and commits every pending
// record in one commit. It refuses an artifact that is not committed or
// has uncommitted changes, and a version that already has an outcome for
// the same content. --waive-pack is refused once the pack has an outcome. Approving the pack imports the roadmap block of
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
	o.By, o.Note = oneLine(o.By), clip(redact.Secrets(oneLine(o.Note)), maxNote)
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
	spec, n, content, err := committedArtifact(ctx, p.Path, rec, o.Gate)
	if err != nil {
		return Decided{}, err
	}
	d := Decided{Gate: o.Gate, Status: status, Version: Version(n), Waived: o.WaivePack}
	if err := checkNew(rec, o.Gate, n, content); err != nil {
		return Decided{}, err
	}
	if err := checkWaiver(rec, o.WaivePack); err != nil {
		return Decided{}, err
	}
	if o.WaivePack && spec.Track != "lite" {
		return Decided{}, fmt.Errorf("only a lite-track project may skip the pack gate, and the committed spec says track %s", orDash(spec.Track))
	}
	var roadmap []byte
	// A graduated project keeps its roadmap in the record: the proposal
	// left the tree with the discovery folder.
	importRoadmap := o.Gate == GatePack && status == GateApproved && rec.Graduated == nil
	d.Roadmap = importRoadmap
	if o.Gate == GatePack {
		d.Folder = PackFolder(rec, n)
		if d.NoPDF, err = missing(ctx, p.Path, d.Folder+"/"+PackPDF); err != nil {
			return Decided{}, err
		}
		if importRoadmap {
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
		if err := checkNew(*rec, o.Gate, n, content); err != nil {
			return err
		}
		if err := checkWaiver(*rec, o.WaivePack); err != nil {
			return err
		}
		gate := &rec.Gates.Spec
		if o.Gate == GatePack {
			gate = &rec.Gates.Pack
		}
		d.Replaced = ""
		if gate.Status != "" && gate.Version == d.Version {
			d.Replaced = gate.Status
		}
		*gate = Gate{Status: status, Version: d.Version, By: o.By, At: stamp(o.Now), Note: o.Note, Waived: gate.Waived, Content: content}
		text := fmt.Sprintf("%s %s %s by %s", o.Gate, d.Version, strings.ReplaceAll(status, "-", " "), o.By)
		if d.Replaced != "" {
			text += fmt.Sprintf("; its content changed since it was %s, which this outcome replaces", strings.ReplaceAll(d.Replaced, "-", " "))
		}
		if o.Gate == GateSpec && status == GateApproved {
			if spec.Type != "" {
				rec.Type = spec.Type
			}
			if spec.Track != "" {
				rec.Track = spec.Track
			}
		}
		if o.WaivePack {
			rec.Gates.Pack = Gate{Waived: true, By: o.By, At: stamp(o.Now), Note: "lite track: pack waived at spec " + d.Version + " approval"}
			text += "; the pack gate is waived for the lite track by the user's choice"
		}
		if o.Gate == GatePack && status == GateApproved {
			if importRoadmap {
				b, err := ParseBlock(roadmap, RoadmapProposal, *rec)
				if err != nil {
					return fmt.Errorf("%s: %w", RoadmapProposalPath, err)
				}
				d.Milestones, d.Dependencies = applyRoadmap(rec, b, "")
			}
			rec.Reapproval = Reapproval{}
			gate.Waived = false
		}
		if o.Note != "" {
			text += ": " + o.Note
		}
		rec.Decisions = append(rec.Decisions, Decision{At: stamp(o.Now), Text: text})
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

// checkNew refuses an outcome for a version that already has one for the
// same content, or for an older version. content is the committed object
// id of version n; an outcome recorded before content was kept is taken
// as being for the same content.
func checkNew(rec Record, gate string, n int, content string) error {
	g := rec.Gates.Spec
	if gate == GatePack {
		g = rec.Gates.Pack
	}
	if g.Status == "" || n > versionNumber(g.Version) {
		return nil
	}
	if n == versionNumber(g.Version) && g.Content != "" && g.Content != content {
		return nil
	}
	return fmt.Errorf("%s %s already has an outcome (%s %s); the runner revises it as a new version first",
		gate, Version(n), g.Status, g.Version)
}

// checkWaiver refuses to waive a pack gate that already has an outcome,
// which the waiver would overwrite.
func checkWaiver(rec Record, waive bool) error {
	if g := rec.Gates.Pack; waive && g.Status != "" {
		return fmt.Errorf("the pack gate already has an outcome (%s %s); --waive-pack is only for a pack not yet decided",
			g.Status, g.Version)
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
// HEAD and its git object id, refusing one that is missing or has
// uncommitted changes. It also returns the committed spec, which every
// gate reads. After graduation the spec is archived and only a pack
// outside the discovery folder can be decided.
func committedArtifact(ctx context.Context, root string, rec Record, gate string) (SpecInfo, int, string, error) {
	if rec.Graduated != nil && gate == GateSpec {
		return SpecInfo{}, 0, "", fmt.Errorf("the spec was archived when the project graduated (tag %s); scope changes go through change requests",
			orDash(rec.Graduated.Tag))
	}
	spec, ok, err := CommittedSpec(ctx, root)
	if rec.Graduated != nil {
		spec, ok, err = SpecInfo{Version: versionNumber(rec.Gates.Spec.Version), Track: rec.Track}, true, nil
	}
	if err != nil {
		return SpecInfo{}, 0, "", err
	}
	n := spec.Version
	if gate == GateSpec {
		if !ok {
			return SpecInfo{}, 0, "", fmt.Errorf("HEAD holds no %s; the spec runner commits it before it reports done", SpecPath)
		}
	} else {
		if n, err = CommittedPackVersion(ctx, root, rec); err != nil {
			return SpecInfo{}, 0, "", err
		}
		if n == 0 {
			return SpecInfo{}, 0, "", fmt.Errorf("HEAD holds no pack under %s/; run the pack stage first", ApprovalFolder(rec))
		}
		if gone, err := missing(ctx, root, PackFolder(rec, n)+"/"+PackHTML); err != nil {
			return SpecInfo{}, 0, "", err
		} else if gone {
			return SpecInfo{}, 0, "", fmt.Errorf("HEAD holds no %s/%s", PackFolder(rec, n), PackHTML)
		}
	}
	rel := artifactPath(rec, gate, n)
	dirty, err := uncommitted(ctx, root, rel)
	if err != nil {
		return SpecInfo{}, 0, "", err
	}
	if dirty {
		return SpecInfo{}, 0, "", fmt.Errorf("%s has changes not committed; decide on a committed version", rel)
	}
	id, err := committedID(ctx, root, rel)
	if err != nil {
		return SpecInfo{}, 0, "", err
	}
	if id == "" {
		return SpecInfo{}, 0, "", fmt.Errorf("HEAD holds no %s", rel)
	}
	return spec, n, id, nil
}

// missing reports whether HEAD lacks the file rel.
func missing(ctx context.Context, root, rel string) (bool, error) {
	out, err := git(ctx, root, "ls-tree", "--name-only", "HEAD", "--", rel)
	return out != rel, err
}

// applyRoadmap replaces the record's milestones with the block's, keeping
// what the kit recorded about each one and the scope of the done and the
// running milestones, except closing, the milestone whose done imports the
// block, which takes its title, features and any date from it. Done
// milestones the block leaves out stay first, as recorded. It adds or updates dependencies by name, ignoring
// case. An update sets only the fields the block gives, so an entry added
// by hand keeps its owner and secret location. It returns how many of each
// the block held.
func applyRoadmap(rec *Record, b Block, closing string) (milestones, dependencies int) {
	// The bare scaffold milestone, started before there was a roadmap,
	// takes its title, date and features from the first roadmap.
	bare := !HasRoadmap(*rec)
	old := map[string]Milestone{}
	for _, m := range rec.Milestones {
		old[m.ID] = m
	}
	next := omittedDone(*rec, b)
	for _, bm := range b.Milestones {
		m := old[bm.ID]
		if bare || bm.ID == closing || (m.Status != MilestoneDone && m.Status != MilestoneInProgress) {
			m.ID, m.Title, m.Features = bm.ID, bm.Title, bm.Features
			if bm.ID != closing {
				m.Planned = time.Time{}
			}
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
		i := slices.IndexFunc(rec.Dependencies, func(d Dependency) bool { return strings.EqualFold(d.Name, bd.Name) })
		if i < 0 {
			rec.Dependencies = append(rec.Dependencies, Dependency{ID: nextID("D", depIDs(rec.Dependencies)), Name: bd.Name, Status: DependencyNeeded})
			i = len(rec.Dependencies) - 1
		}
		d := &rec.Dependencies[i]
		for _, f := range []struct {
			to   *string
			from string
		}{
			{&d.Owner, bd.Owner}, {&d.Milestone, bd.Milestone}, {&d.Workaround, bd.Workaround}, {&d.SecretLocation, bd.SecretLocation},
		} {
			if f.from != "" {
				*f.to = f.from
			}
		}
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

// AddDecision keeps a decision of the user in the record, with values that
// look like secrets hidden. It is committed with the next gate outcome or
// milestone.
func AddDecision(s *state.Store, name, text string, now time.Time) error {
	text = redact.Secrets(strings.TrimSpace(text))
	if text == "" {
		return errors.New("the decision is empty")
	}
	_, err := Update(s, name, func(rec *Record) error {
		rec.Decisions = append(rec.Decisions, Decision{At: stamp(now), Text: text})
		return nil
	})
	return err
}
