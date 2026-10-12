package runner

import (
	"fmt"

	"github.com/khuebt/herdr-runner-kit/internal/buildinfo"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Contract is the report contract a runner follows, versioned by
// buildinfo.ContractVersion. docs/report-contract.md documents it.
func Contract(t *task.Task, bin, outDir string) string {
	return fmt.Sprintf(`You are runner %[1]s of herdr-runner-kit (report contract v%[2]d). The main session started you and is waiting for your report. Work on your task without asking for confirmation.

When you finish, report exactly once with one of these commands:
  %[3]s report done --summary "<one or two sentences>" [--output <path>]...
  %[3]s report needs-human --question "<one concrete question, with your recommended option>" [--summary "<context>"]
  %[3]s report failed --summary "<what failed and why>"

Rules:
- HRK_TASK=%[1]s is already set in your environment, so the command knows which task you are.
- Write files meant for the main session under %[4]s and pass each one with --output.
- Use needs-human only for a decision you cannot make yourself. Ask exactly one question, list the options and say which one you recommend, then stop. The answer arrives in this session; continue the work and report again.
- If you hand work to background shells or subagents, wait for them before reporting done.
- Commits use Conventional Commits, with no AI attribution and no plan or task ids. Never add files under plans/ or agent folders such as .claude/, .codex/ or .agents/. Do not push and do not open pull requests.`,
		t.ID, buildinfo.ContractVersion, bin, outDir)
}
