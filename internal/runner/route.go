package runner

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// tagPattern matches a leading "#t3" routing tag.
var tagPattern = regexp.MustCompile(`(?i)^#(t\d+)\b[\s:,.-]*`)

// metaMessages are untagged questions for the main agent itself.
var metaMessages = []string{"?", "status", "status?", "list", "list?", "help"}

// approvals release a held task without a note. A task held with a reason
// is released only by a reply tagged with its id.
var approvals = []string{"ok", "okay", "yes", "y", "go", "approve", "approved", "release", "duyệt", "đồng ý", "có"}

// RouteTelegram delivers a Telegram reply that reached the main session. A
// leading #id picks the task. Without one, the text goes to the only task
// waiting for a human, from any owner, because one main session holds
// Telegram for all of them. It returns the line to show the main agent.
func (r *Runner) RouteTelegram(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("empty reply")
	}
	id, body := "", text
	m := tagPattern.FindStringSubmatch(text)
	if m != nil {
		id, body = strings.ToLower(m[1]), strings.TrimSpace(text[len(m[0]):])
	} else {
		if slices.Contains(metaMessages, strings.ToLower(text)) {
			return "meta: not for a runner; answer it yourself", nil
		}
		// A gate approval is the user's call, recorded by the main session;
		// it never releases or answers a runner.
		if line, ok := r.gateApproval(text); ok {
			return line, nil
		}
		waiting, err := r.waitingForHuman()
		if err != nil {
			return "", err
		}
		if name, ok := r.projectStatusRequest(text); ok {
			// A runner waiting for a human may have asked something that
			// "status <name>" answers, so never take its reply away.
			if len(waiting) == 0 {
				return "meta: project status " + name, nil
			}
			return `ambiguous: "status ` + name + `" could be an answer for ` + strings.Join(waiting, ", ") + "; ask, or show project status", nil
		}
		switch len(waiting) {
		case 0:
			return "ambiguous: no task is waiting; ask which task this is for", nil
		case 1:
			id = waiting[0]
		default:
			return "ambiguous: waiting " + strings.Join(waiting, ", ") + "; ask for a #id tag", nil
		}
	}
	t, err := r.Store.Get(id)
	if err != nil {
		return "", err
	}
	switch {
	case t.Status == task.Held && t.HoldReason != "" && m == nil:
		// A task held for a reason, such as a dependency, waits for the
		// user to name it; a bare "ok" may be meant for something else.
		return fmt.Sprintf("ambiguous: %s is held (%s); release it with \"#%s ok\" once that is settled", id, t.HoldReason, id), nil
	case t.Status == task.Held:
		note := body
		if isApproval(body) {
			note = ""
		}
		if _, err := r.Release(id, note); err != nil {
			return "", err
		}
		return id + " released", nil
	case body == "":
		return "", fmt.Errorf("reply to %s has no text after the tag", id)
	case t.Status == task.NeedsHuman && t.Question != "":
		working, err := r.Answer(ctx, id, body)
		if err != nil {
			return "", err
		}
		return deliveredLine(id, "got the answer", working), nil
	case t.Status == task.Running:
		working, err := r.Send(ctx, id, body)
		if err != nil {
			return "", err
		}
		return deliveredLine(id, "received the message", working), nil
	case t.Status == task.NeedsHuman:
		return "", fmt.Errorf("%s waits on the %s dialog in its herdr tab; it cannot be answered from Telegram", id, t.Error)
	}
	return "", fmt.Errorf("%s is %s; nothing to answer", id, t.Status)
}

// projectStatusRequest reports whether text is "status <name>" for a
// registered project, and returns the name. A registry that cannot be read
// holds no project here, so replies are routed as if there were none.
func (r *Runner) projectStatusRequest(text string) (string, bool) {
	f := strings.Fields(strings.ToLower(strings.TrimRight(text, "?")))
	if len(f) != 2 || f[0] != "status" || state.CheckProjectName(f[1]) != nil {
		return "", false
	}
	p, err := r.Store.ProjectByName(f[1])
	if err != nil {
		return "", false
	}
	return p.Name, !p.Reserved
}

// gateApprovalPattern matches "approve spec <name>" and
// "approve pack <name>".
var gateApprovalPattern = regexp.MustCompile(`(?i)^approve\s+(spec|pack)\s+(\S+?)[.!]?$`)

// gateApproval returns the meta line for an untagged gate approval.
func (r *Runner) gateApproval(text string) (string, bool) {
	m := gateApprovalPattern.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	gate, name := strings.ToLower(m[1]), strings.ToLower(m[2])
	if state.CheckProjectName(name) == nil {
		if p, err := r.Store.ProjectByName(name); err == nil && !p.Reserved {
			return fmt.Sprintf("meta: user approves %s of %s; confirm and run project approve %s --project %s", gate, p.Name, gate, p.Name), true
		}
	}
	return fmt.Sprintf("meta: user approves %s of %q, which is not a registered project; ask which project they mean", gate, name), true
}

// waitingForHuman lists tasks that a reply can resolve: a runner's question
// or a held gate.
func (r *Runner) waitingForHuman() ([]string, error) {
	tasks, err := r.Store.List()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, t := range tasks {
		if t.Status == task.Held || (t.Status == task.NeedsHuman && t.Question != "") {
			ids = append(ids, t.ID)
		}
	}
	return ids, nil
}

func isApproval(s string) bool {
	return slices.Contains(approvals, strings.ToLower(strings.Trim(strings.TrimSpace(s), ".!"))) || s == ""
}

func deliveredLine(id, what string, working bool) string {
	if working {
		return id + " " + what + " and is working"
	}
	return id + " " + what + "; herdr has not seen it start a turn yet"
}
