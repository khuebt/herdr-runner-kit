package task

import (
	"fmt"
	"slices"
	"strings"
)

// Harness is the agent CLI a runner uses.
type Harness string

// Supported harnesses.
const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
)

// Effort levels each harness accepts, taken from `claude --help` and the Codex
// model_reasoning_effort setting.
var effortLevels = map[Harness][]string{
	Claude: {"low", "medium", "high", "xhigh", "max"},
	Codex:  {"minimal", "low", "medium", "high", "xhigh"},
}

// AgentSpec selects the harness, model and effort of a runner. An empty model
// or effort leaves the choice to the CLI's own configuration.
type AgentSpec struct {
	Harness Harness
	Model   string
	Effort  string
}

// ParseAgentSpec parses "harness[:model[:effort]]", such as "codex",
// "claude:claude-opus-5-5:high" or "claude::max".
func ParseAgentSpec(s string) (AgentSpec, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) > 3 {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: want harness[:model[:effort]]", s)
	}
	spec := AgentSpec{Harness: Harness(parts[0])}
	if len(parts) > 1 {
		spec.Model = parts[1]
	}
	if len(parts) > 2 {
		spec.Effort = parts[2]
	}
	levels, ok := effortLevels[spec.Harness]
	if !ok {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: harness must be claude or codex", s)
	}
	if strings.ContainsAny(spec.Model, " \t\n") {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: model must not contain spaces", s)
	}
	if spec.Effort != "" && !slices.Contains(levels, spec.Effort) {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: %s effort must be one of %s",
			s, spec.Harness, strings.Join(levels, ", "))
	}
	return spec, nil
}

// String returns the shortest form that parses back to the same spec.
func (a AgentSpec) String() string {
	switch {
	case a.Effort != "":
		return string(a.Harness) + ":" + a.Model + ":" + a.Effort
	case a.Model != "":
		return string(a.Harness) + ":" + a.Model
	default:
		return string(a.Harness)
	}
}
