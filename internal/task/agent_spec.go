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
	Pi     Harness = "pi"
	Omp    Harness = "omp" // oh-my-pi
)

// harnessInfo is what the agent spec needs to know about one harness.
type harnessInfo struct {
	efforts    []string // the effort levels it accepts
	needsModel bool     // a spec must name the model
}

// harnesses lists every supported harness in the order the kit shows them.
// Effort levels come from `claude --help`, the Codex model_reasoning_effort
// setting and the --thinking levels of pi and omp. pi and omp need a model,
// because without one omp picks whichever provider it finds first.
var harnesses = []struct {
	name Harness
	harnessInfo
}{
	{Claude, harnessInfo{efforts: []string{"low", "medium", "high", "xhigh", "max"}}},
	{Codex, harnessInfo{efforts: []string{"minimal", "low", "medium", "high", "xhigh"}}},
	{Pi, harnessInfo{efforts: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, needsModel: true}},
	{Omp, harnessInfo{efforts: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"}, needsModel: true}},
}

// Harnesses returns the supported harnesses in order.
func Harnesses() []Harness {
	out := make([]Harness, len(harnesses))
	for i, h := range harnesses {
		out[i] = h.name
	}
	return out
}

func infoOf(h Harness) (harnessInfo, bool) {
	for _, e := range harnesses {
		if e.name == h {
			return e.harnessInfo, true
		}
	}
	return harnessInfo{}, false
}

// harnessChoice lists the harnesses as "a, b or c".
func harnessChoice() string {
	names := make([]string, len(harnesses))
	for i, h := range harnesses {
		names[i] = string(h.name)
	}
	last := len(names) - 1
	if last == 0 {
		return names[0]
	}
	return strings.Join(names[:last], ", ") + " or " + names[last]
}

// AgentSpec selects the harness, model and effort of a runner. An empty model
// or effort leaves the choice to the CLI's own configuration.
type AgentSpec struct {
	Harness Harness
	Model   string
	Effort  string
}

// ParseAgentSpec parses "harness[:model[:effort]]", such as "codex",
// "claude:claude-opus-5-5:high", "claude::max" or
// "pi:openai-codex/gpt-6-luna:high".
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
	info, ok := infoOf(spec.Harness)
	if !ok {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: harness must be %s", s, harnessChoice())
	}
	if info.needsModel && spec.Model == "" {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: %s needs a model, such as %s:openai-codex/gpt-6-luna[:effort]", s, spec.Harness, spec.Harness)
	}
	if strings.ContainsAny(spec.Model, " \t\n") {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: model must not contain spaces", s)
	}
	if spec.Effort != "" && !slices.Contains(info.efforts, spec.Effort) {
		return AgentSpec{}, fmt.Errorf("invalid agent %q: %s effort must be one of %s",
			s, spec.Harness, strings.Join(info.efforts, ", "))
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
