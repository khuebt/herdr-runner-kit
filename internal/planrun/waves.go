package planrun

import (
	"fmt"
	"path"
	"strings"
)

// Waves orders the phases that are not done into waves. Every phase of a
// wave is ready once the earlier waves are done, so a wave can run at once.
// While no phase is done, the first ready phase runs alone, because it
// creates the run's checkout. After that, ready phases fill a wave in plan
// order while it holds fewer than maxParallel phases and every scope in it
// is declared and disjoint from the others.
func Waves(plan *Plan, maxParallel int, done func(int) bool) ([][]Phase, error) {
	maxParallel = max(maxParallel, 1)
	var left []Phase
	for _, ph := range plan.Phases {
		if !done(ph.Number) {
			left = append(left, ph)
		}
	}
	placed := map[int]bool{}
	first := len(left) == len(plan.Phases)
	var waves [][]Phase
	for len(placed) < len(left) {
		var wave []Phase
		for _, ph := range left {
			if placed[ph.Number] || !ready(ph, done, placed) {
				continue
			}
			if len(wave) > 0 && (first || !joins(wave, ph, maxParallel)) {
				continue
			}
			wave = append(wave, ph)
		}
		if len(wave) == 0 {
			var stuck []string
			for _, ph := range left {
				if !placed[ph.Number] {
					stuck = append(stuck, fmt.Sprint(ph.Number))
				}
			}
			return nil, fmt.Errorf("phases %s wait on each other", strings.Join(stuck, ", "))
		}
		for _, ph := range wave {
			placed[ph.Number] = true
		}
		waves = append(waves, wave)
		first = false
	}
	return waves, nil
}

// ready reports whether every dependency of ph is done or sits in an
// earlier wave.
func ready(ph Phase, done func(int) bool, placed map[int]bool) bool {
	for _, d := range ph.Deps {
		if !done(d) && !placed[d] {
			return false
		}
	}
	return true
}

// joins reports whether ph may run alongside the phases of wave.
func joins(wave []Phase, ph Phase, maxParallel int) bool {
	if len(wave) >= maxParallel || len(ph.Scope) == 0 {
		return false
	}
	for _, other := range wave {
		if len(other.Scope) == 0 || Overlaps(other.Scope, ph.Scope) {
			return false
		}
	}
	return true
}

// Overlaps reports whether two scopes may touch the same file. When it
// cannot tell, as for a bare file name against a folder, it says they do.
func Overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if entriesOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

func entriesOverlap(a, b string) bool {
	pa, aWide := prefix(a)
	pb, bWide := prefix(b)
	aBare, bBare := !strings.Contains(a, "/"), !strings.Contains(b, "/")
	switch {
	case aWide && bWide:
		return strings.HasPrefix(pa, pb) || strings.HasPrefix(pb, pa)
	case aWide:
		return bBare || InScope([]string{a}, b)
	case bWide:
		return aBare || InScope([]string{b}, a)
	case aBare || bBare:
		return path.Base(a) == path.Base(b)
	default:
		return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
	}
}

// prefix returns the literal start of a folder or glob entry, and whether
// the entry is one.
func prefix(e string) (string, bool) {
	if i := strings.IndexByte(e, '*'); i >= 0 {
		return e[:i], true
	}
	if strings.HasSuffix(e, "/") {
		return e, true
	}
	return e, false
}

// InScope reports whether the repo-relative file falls inside scope.
func InScope(scope []string, file string) bool {
	for _, e := range scope {
		switch {
		case strings.Contains(e, "*"):
			if ok, _ := path.Match(e, file); ok {
				return true
			}
			if strings.Contains(e, "**") && strings.HasPrefix(file, e[:strings.Index(e, "**")]) {
				return true
			}
		case strings.HasSuffix(e, "/"):
			if strings.HasPrefix(file, e) {
				return true
			}
		case !strings.Contains(e, "/"):
			if path.Base(file) == e {
				return true
			}
		default:
			if file == e || strings.HasPrefix(file, e+"/") {
				return true
			}
		}
	}
	return false
}
