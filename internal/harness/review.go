package harness

import "github.com/khuebt/herdr-runner-kit/internal/task"

// reviewers are the agents a review runs on by default: each harness's
// strongest model at high effort; Codex's model comes from its own config.
var reviewers = map[task.Harness]task.AgentSpec{
	task.Claude: {Harness: task.Claude, Model: "claude-opus-5-5", Effort: "high"},
	task.Codex:  {Harness: task.Codex, Effort: "high"},
}

// DefaultReviewer picks the agent that reviews the work of writer: Codex
// for a Claude writer and Claude for any other, when that harness's
// command is installed; otherwise writer's own harness on another model
// at high effort. installed reports whether a command is on PATH.
func DefaultReviewer(writer task.AgentSpec, installed func(bin string) bool) task.AgentSpec {
	other := task.Claude
	if writer.Harness == task.Claude {
		other = task.Codex
	}
	if a, err := For(other); err == nil && installed(a.Checks().Bin) {
		return reviewers[other]
	}
	if writer.Harness == task.Claude {
		spec := reviewers[task.Claude]
		if writer.Model == spec.Model {
			spec.Model = "claude-sonnet-5-5"
		}
		return spec
	}
	// ponytail: the kit knows no other model of these harnesses, so the
	// reviewer may match the writer's model; a config review.agent is the way out.
	return task.AgentSpec{Harness: writer.Harness, Model: writer.Model, Effort: "high"}
}
