// Package planrun reads an AgentKit plan, a plan.md next to phase-NN-*.md
// files, and orders its phases into waves a runner per phase can work in.
package planrun

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// PlanFile is the name of a plan's index file inside its folder.
const PlanFile = "plan.md"

// Values of plan.md's review key.
const (
	ReviewRequired = "required"
	ReviewSkip     = "skip"
)

// Plan is an AgentKit plan folder.
type Plan struct {
	Title  string
	Status string
	// Dir is the absolute plan folder and File its plan.md.
	Dir    string
	File   string
	Phases []Phase
	// Review is ReviewRequired, ReviewSkip or empty (no review) from
	// plan.md's review key. ReviewAfter is the phase after whose wave the
	// first review runs, 0 for none; ReviewAgent overrides the reviewer's
	// agent spec.
	Review      string
	ReviewAfter int
	ReviewAgent string
}

// Phase is one phase file of a plan.
type Phase struct {
	Number int
	Title  string
	Status string
	// File is the absolute phase file.
	File string
	// Deps are the numbers of the phases this one needs done first.
	Deps []int
	// Scope lists the repo-relative paths the phase declares it writes. A
	// path ending in "/" is a folder, one holding "*" a glob, and one with
	// no "/" a file name anywhere in the repository. Empty means the phase
	// declared none.
	Scope []string
}

// Phase returns the phase numbered n.
func (p *Plan) Phase(n int) (Phase, bool) {
	for _, ph := range p.Phases {
		if ph.Number == n {
			return ph, true
		}
	}
	return Phase{}, false
}

type frontmatter struct {
	Phase        any    `yaml:"phase"`
	Title        string `yaml:"title"`
	Status       string `yaml:"status"`
	Dependencies *[]any `yaml:"dependencies"`
	Review       string `yaml:"review"`
	ReviewAfter  any    `yaml:"review_after"`
	ReviewAgent  string `yaml:"review_agent"`
}

var phaseFileRe = regexp.MustCompile(`^phase-(\d+)`)

// Read reads the plan at path, a plan.md file or its folder.
func Read(p string) (*Plan, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil {
		return nil, err
	} else if info.IsDir() {
		abs = filepath.Join(abs, PlanFile)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	fm, body, err := parseFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	plan := &Plan{Title: fm.Title, Status: fm.Status, Dir: filepath.Dir(abs), File: abs}
	if plan.Title == "" {
		plan.Title = firstHeading(body)
	}
	files, err := filepath.Glob(filepath.Join(plan.Dir, "phase-*.md"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("plan %s has no phase-*.md files", plan.Dir)
	}
	seen := map[int]string{}
	var explicit []bool
	for _, f := range files {
		ph, hasDeps, err := readPhase(f)
		if err != nil {
			return nil, err
		}
		if other, dup := seen[ph.Number]; dup {
			return nil, fmt.Errorf("phase %d is declared by both %s and %s", ph.Number, filepath.Base(other), filepath.Base(f))
		}
		seen[ph.Number] = f
		plan.Phases = append(plan.Phases, ph)
		explicit = append(explicit, hasDeps)
	}
	idx := make([]int, len(plan.Phases))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return plan.Phases[idx[a]].Number < plan.Phases[idx[b]].Number })
	sorted := make([]Phase, len(idx))
	for i, j := range idx {
		sorted[i] = plan.Phases[j]
		if !explicit[j] && i > 0 {
			sorted[i].Deps = []int{sorted[i-1].Number}
		}
	}
	plan.Phases = sorted
	if err := checkDeps(plan.Phases); err != nil {
		return nil, err
	}
	if err := readReview(plan, fm); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	return plan, nil
}

// readReview validates plan.md's review keys and stores them on plan.
func readReview(plan *Plan, fm frontmatter) error {
	switch fm.Review {
	case "", ReviewRequired, ReviewSkip:
		plan.Review = fm.Review
	default:
		return fmt.Errorf("review must be %s or %s, got %q", ReviewRequired, ReviewSkip, fm.Review)
	}
	if fm.ReviewAfter != nil {
		n, ok := phaseNumber(fm.ReviewAfter)
		if !ok {
			return fmt.Errorf("review_after %v is not a phase number", fm.ReviewAfter)
		}
		if _, ok := plan.Phase(n); !ok {
			return fmt.Errorf("review_after names phase %d, which the plan does not have", n)
		}
		if plan.Review != ReviewRequired {
			return fmt.Errorf("review_after needs review: %s", ReviewRequired)
		}
		plan.ReviewAfter = n
	}
	if fm.ReviewAgent != "" {
		spec, err := task.ParseAgentSpec(fm.ReviewAgent)
		if err != nil {
			return fmt.Errorf("review_agent: %w", err)
		}
		plan.ReviewAgent = spec.String()
	}
	return nil
}

func readPhase(file string) (Phase, bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Phase{}, false, err
	}
	fm, body, err := parseFrontmatter(data)
	if err != nil {
		return Phase{}, false, fmt.Errorf("%s: %w", file, err)
	}
	ph := Phase{Title: fm.Title, Status: fm.Status, File: file, Scope: Scope(body)}
	if ph.Title == "" {
		ph.Title = firstHeading(body)
	}
	switch {
	case fm.Phase != nil:
		n, ok := phaseNumber(fm.Phase)
		if !ok {
			return Phase{}, false, fmt.Errorf("%s: phase %v is not a number", file, fm.Phase)
		}
		ph.Number = n
	default:
		m := phaseFileRe.FindStringSubmatch(filepath.Base(file))
		if m == nil {
			return Phase{}, false, fmt.Errorf("%s: no phase number in its frontmatter or name", file)
		}
		ph.Number, _ = strconv.Atoi(m[1])
	}
	if fm.Dependencies == nil {
		return ph, false, nil
	}
	ph.Deps = []int{}
	for _, d := range *fm.Dependencies {
		n, ok := phaseNumber(d)
		if !ok {
			return Phase{}, false, fmt.Errorf("phase %d (%s): dependency %v is not a phase number", ph.Number, filepath.Base(file), d)
		}
		ph.Deps = append(ph.Deps, n)
	}
	return ph, true, nil
}

// phaseNumber reads an int, or a string that parses as one.
func phaseNumber(v any) (int, bool) {
	switch v := v.(type) {
	case int:
		return v, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return n, err == nil
	}
	return 0, false
}

// checkDeps refuses a dependency on an unknown phase and a cycle.
func checkDeps(phases []Phase) error {
	byNum := map[int]Phase{}
	for _, ph := range phases {
		byNum[ph.Number] = ph
	}
	for _, ph := range phases {
		for _, d := range ph.Deps {
			if _, ok := byNum[d]; !ok {
				return fmt.Errorf("phase %d depends on phase %d, which the plan does not have", ph.Number, d)
			}
		}
	}
	const (
		unseen = iota
		visiting
		finished
	)
	mark := map[int]int{}
	var visit func(n int, trail []int) error
	visit = func(n int, trail []int) error {
		switch mark[n] {
		case finished:
			return nil
		case visiting:
			start := 0
			for i, t := range trail {
				if t == n {
					start = i
				}
			}
			var names []string
			for _, t := range append(trail[start:], n) {
				names = append(names, strconv.Itoa(t))
			}
			return fmt.Errorf("phase dependencies form a cycle: %s", strings.Join(names, " -> "))
		}
		mark[n] = visiting
		for _, d := range byNum[n].Deps {
			if err := visit(d, append(trail, n)); err != nil {
				return err
			}
		}
		mark[n] = finished
		return nil
	}
	for _, ph := range phases {
		if err := visit(ph.Number, nil); err != nil {
			return err
		}
	}
	return nil
}

// parseFrontmatter splits a leading YAML block between "---" lines from the
// body. A file without one has an empty frontmatter.
func parseFrontmatter(data []byte) (frontmatter, []byte, error) {
	var fm frontmatter
	start, end, ok := frontmatterBounds(data)
	if !ok {
		return fm, data, nil
	}
	if err := yaml.Unmarshal(data[start:end], &fm); err != nil {
		return fm, nil, fmt.Errorf("frontmatter: %w", err)
	}
	rest := data[end:]
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		rest = nil
	}
	return fm, rest, nil
}

// frontmatterBounds returns the byte range of the YAML between a leading
// "---" line and the next "---" line; end is where the closing line starts.
func frontmatterBounds(data []byte) (start, end int, ok bool) {
	first, rest, found := bytes.Cut(data, []byte("\n"))
	if !found || string(bytes.TrimRight(first, "\r")) != "---" {
		return 0, 0, false
	}
	start = len(first) + 1
	pos := start
	for len(rest) > 0 {
		line, next, _ := bytes.Cut(rest, []byte("\n"))
		if string(bytes.TrimRight(line, "\r")) == "---" {
			return start, pos, true
		}
		pos += len(line) + 1
		rest = next
	}
	return 0, 0, false
}

func firstHeading(body []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		if t, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "# "); ok {
			return strings.TrimSpace(t)
		}
	}
	return ""
}

var (
	headingRe  = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	codeSpanRe = regexp.MustCompile("`([^`]+)`")
	lineSufRe  = regexp.MustCompile(`:\d+(-\d+)?$`)
	pathRe     = regexp.MustCompile(`^[A-Za-z0-9_.*@+/-]+$`)
	extRe      = regexp.MustCompile(`[^./]\.[a-z0-9]{1,8}$`)
	// moduleHostRe matches the host that starts a module path, such as
	// "gopkg.in" or "github.com".
	moduleHostRe = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*\.(com|in|io|org|net|dev)$`)
)

// Scope returns the write scope a phase body declares: the backticked
// repo-relative paths in sections whose heading names files, in order and
// without repeats.
func Scope(body []byte) []string {
	var scope []string
	seen := map[string]bool{}
	active, fenced := 0, false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			if active > 0 && level <= active {
				active = 0
			}
			if active == 0 && strings.Contains(strings.ToLower(m[2]), "file") {
				active = level
			}
			continue
		}
		if active == 0 {
			continue
		}
		for _, m := range codeSpanRe.FindAllStringSubmatch(line, -1) {
			if p, ok := scopePath(m[1]); ok && !seen[p] {
				seen[p] = true
				scope = append(scope, p)
			}
		}
	}
	return scope
}

// scopePath turns a code span into a scope path, or reports that it is not
// a repo-relative path.
func scopePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	// A placeholder such as "testdata/<small plans>/" names a folder.
	if i := strings.Index(s, "/<"); i > 0 {
		s = s[:i+1]
	}
	s = lineSufRe.ReplaceAllString(s, "")
	// A Go module path such as "gopkg.in/yaml.v3" is no repository path.
	if first, _, ok := strings.Cut(s, "/"); ok && moduleHostRe.MatchString(first) {
		return "", false
	}
	if s == "" || !pathRe.MatchString(s) || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "-") {
		return "", false
	}
	dir := strings.HasSuffix(s, "/")
	if !dir && !strings.Contains(s, "*") && !extRe.MatchString(s) && !strings.Contains(strings.Trim(s, "/"), "/") {
		return "", false
	}
	for _, seg := range strings.Split(strings.Trim(s, "/"), "/") {
		if seg == ".." {
			return "", false
		}
	}
	s = path.Clean(s)
	if s == "." {
		return "", false
	}
	if dir {
		s += "/"
	}
	return s, true
}
