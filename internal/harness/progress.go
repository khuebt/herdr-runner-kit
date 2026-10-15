package harness

import (
	"regexp"
	"strings"
	"time"
)

// SelfResolvingPrompt is a prompt that blocks the agent until the agent
// answers it itself after Timeout, so a block on it needs no one.
type SelfResolvingPrompt struct {
	Name    string
	Match   string // phrase on the visible screen, compared after Normalize
	Timeout time.Duration
}

// MatchSelfResolving returns the first of a's self-resolving prompts whose
// text is on screen.
func MatchSelfResolving(a Adapter, screen string) (SelfResolvingPrompt, bool) {
	text := Normalize(screen)
	for _, p := range a.SelfResolvingPrompts() {
		if strings.Contains(text, Normalize(p.Match)) {
			return p, true
		}
	}
	return SelfResolvingPrompt{}, false
}

// durations matches the elapsed-time counters agents draw while a turn or
// a tool runs, such as "14s", "1m 30s" or "(42s)", a compound counter as
// one match, so it masks the same way before and after it reaches a minute.
var durations = regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ms|[smh])(?:\s+\d+(?:\.\d+)?(?:ms|[smh]))*\b`)

// maskDurations hides elapsed-time counters, which change every second
// whether or not anything happens.
func maskDurations(s string) string { return durations.ReplaceAllString(s, "#") }

// tokenCount matches the output token counter of a streaming turn.
var tokenCount = regexp.MustCompile(`[\d.,]+k?\s+tokens`)

// statusLines is how many of the screen's last lines hold an agent's own
// status, such as its retry line.
const statusLines = 12

// lastLines returns the last n lines of screen without trailing blank ones.
func lastLines(screen string, n int) []string {
	lines := strings.Split(strings.TrimRight(screen, " \t\r\n"), "\n")
	return lines[max(0, len(lines)-n):]
}

// anyLineMatches reports whether one of lines matches re.
func anyLineMatches(lines []string, re *regexp.Regexp) bool {
	for _, l := range lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

// joinLines puts lines back together as screen text, without blank lines,
// which agents add and drop around their status lines as they redraw.
func joinLines(lines []string) string {
	out := lines[:0:0]
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
