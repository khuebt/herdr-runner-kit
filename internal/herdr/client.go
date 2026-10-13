// Package herdr is a thin wrapper over the herdr CLI. All parsing of herdr's
// JSON lives here, so a change in herdr's output touches one file.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds every herdr call that has no timeout of its own.
const DefaultTimeout = 15 * time.Second

// Error codes herdr returns that callers branch on.
const (
	CodeAgentNotReady   = "agent_not_ready"
	CodeAgentNotRunning = "agent_not_running"
	CodeAgentNotFound   = "agent_not_found"
	CodeTimeout         = "timeout"
	CodePromptStalled   = "agent_prompt_stalled"
	CodeAgentBlocked    = "agent_blocked"
)

// Error is a herdr CLI failure. Code is herdr's error code when it printed
// one, else empty.
type Error struct {
	Args    []string
	Code    string
	Message string
}

func (e *Error) Error() string {
	cmd := "herdr " + strings.Join(e.Args[:min(2, len(e.Args))], " ")
	if e.Code != "" {
		return fmt.Sprintf("%s: %s: %s", cmd, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s", cmd, e.Message)
}

// HasCode reports whether err is a herdr error with the given code.
func HasCode(err error, code string) bool {
	var he *Error
	return errors.As(err, &he) && he.Code == code
}

// ExecFunc runs the herdr binary and returns its stdout and stderr.
type ExecFunc func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)

// Client calls the herdr CLI.
type Client struct {
	Bin  string
	Exec ExecFunc
}

// New returns a client for the herdr binary on PATH.
func New() *Client {
	return &Client{Bin: "herdr", Exec: execCommand}
}

func execCommand(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, err := c.Exec(ctx, c.Bin, args...)
	if err == nil {
		return stdout, nil
	}
	var resp struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if json.Unmarshal(bytes.TrimSpace(stderr), &resp) == nil && resp.Error.Code != "" {
		return nil, &Error{Args: args, Code: resp.Error.Code, Message: resp.Error.Message}
	}
	msg := strings.TrimSpace(string(stderr))
	if ctx.Err() != nil {
		msg = "no answer within " + timeout.String()
	}
	if msg == "" {
		msg = err.Error()
	}
	return nil, &Error{Args: args, Message: msg}
}

func (c *Client) runJSON(ctx context.Context, timeout time.Duration, out any, args ...string) error {
	data, err := c.run(ctx, timeout, args...)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return &Error{Args: args, Message: "unexpected output: " + err.Error()}
	}
	return nil
}

// Location is where a runner lives in herdr.
type Location struct {
	WorkspaceID string
	TabID       string
	PaneID      string
	// Worktree fields are set only by WorktreeCreate.
	Path   string
	Branch string
}

type paneJSON struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// TabCreate opens a tab in workspace, without taking focus.
func (c *Client) TabCreate(ctx context.Context, workspace, cwd, label string, env []string) (Location, error) {
	args := []string{"tab", "create", "--workspace", workspace, "--cwd", cwd, "--label", label, "--no-focus"}
	for _, kv := range env {
		args = append(args, "--env", kv)
	}
	var resp struct {
		Result struct {
			RootPane paneJSON `json:"root_pane"`
		} `json:"result"`
	}
	if err := c.runJSON(ctx, DefaultTimeout, &resp, args...); err != nil {
		return Location{}, err
	}
	p := resp.Result.RootPane
	return Location{WorkspaceID: p.WorkspaceID, TabID: p.TabID, PaneID: p.PaneID}, nil
}

// WorktreeOptions selects the repository by an open workspace or by a path.
type WorktreeOptions struct {
	Workspace string
	Cwd       string
	Branch    string
	Base      string
	Label     string
}

// WorktreeCreate creates a git worktree and opens it as its own workspace.
func (c *Client) WorktreeCreate(ctx context.Context, o WorktreeOptions) (Location, error) {
	args := []string{"worktree", "create"}
	if o.Workspace != "" {
		args = append(args, "--workspace", o.Workspace)
	} else {
		args = append(args, "--cwd", o.Cwd)
	}
	args = append(args, "--branch", o.Branch)
	if o.Base != "" {
		args = append(args, "--base", o.Base)
	}
	args = append(args, "--label", o.Label, "--no-focus")
	var resp struct {
		Result struct {
			RootPane paneJSON `json:"root_pane"`
			Worktree struct {
				Path   string `json:"path"`
				Branch string `json:"branch"`
			} `json:"worktree"`
		} `json:"result"`
	}
	if err := c.runJSON(ctx, DefaultTimeout, &resp, args...); err != nil {
		return Location{}, err
	}
	p := resp.Result.RootPane
	return Location{
		WorkspaceID: p.WorkspaceID, TabID: p.TabID, PaneID: p.PaneID,
		Path: resp.Result.Worktree.Path, Branch: resp.Result.Worktree.Branch,
	}, nil
}

// WorktreeRemove removes a worktree workspace. Without force, herdr refuses
// a dirty worktree.
func (c *Client) WorktreeRemove(ctx context.Context, workspace string, force bool) error {
	args := []string{"worktree", "remove", "--workspace", workspace}
	if force {
		args = append(args, "--force")
	}
	return c.runJSON(ctx, DefaultTimeout, nil, args...)
}

// workspaceWorktree is the git checkout herdr reports for a workspace.
type workspaceWorktree struct {
	CheckoutPath string `json:"checkout_path"`
	RepoRoot     string `json:"repo_root"`
	Linked       bool   `json:"is_linked_worktree"`
}

// workspaceWorktree returns the checkout of workspace id, or nil when the
// workspace is not in a git repository.
func (c *Client) workspaceWorktree(ctx context.Context, id string) (*workspaceWorktree, error) {
	var get struct {
		Result struct {
			Workspace struct {
				Worktree *workspaceWorktree `json:"worktree"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if err := c.runJSON(ctx, DefaultTimeout, &get, "workspace", "get", id); err != nil {
		return nil, err
	}
	return get.Result.Workspace.Worktree, nil
}

// WorkspaceCheckout returns the path of the git checkout workspace id is
// open in, or "" when it is not in a git repository. herdr reuses the ids of
// closed workspaces, so callers check this before acting on an old id.
func (c *Client) WorkspaceCheckout(ctx context.Context, id string) (string, error) {
	wt, err := c.workspaceWorktree(ctx, id)
	if err != nil || wt == nil {
		return "", err
	}
	return wt.CheckoutPath, nil
}

// RepoWorkspace returns the open, non-linked workspace of the repository at
// root, or "" when none is open.
func (c *Client) RepoWorkspace(ctx context.Context, root string) (string, error) {
	var list struct {
		Result struct {
			Workspaces []struct {
				ID string `json:"workspace_id"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if err := c.runJSON(ctx, DefaultTimeout, &list, "workspace", "list"); err != nil {
		return "", err
	}
	for _, ws := range list.Result.Workspaces {
		wt, err := c.workspaceWorktree(ctx, ws.ID)
		if err != nil {
			continue // closed in between
		}
		if wt != nil && !wt.Linked && wt.RepoRoot == root {
			return ws.ID, nil
		}
	}
	return "", nil
}

// TabClose closes a tab and ends the processes in it.
func (c *Client) TabClose(ctx context.Context, tab string) error {
	return c.runJSON(ctx, DefaultTimeout, nil, "tab", "close", tab)
}

// PaneRun types command and Enter into a pane's shell.
func (c *Client) PaneRun(ctx context.Context, pane, command string) error {
	return c.runJSON(ctx, DefaultTimeout, nil, "pane", "run", pane, command)
}

// PaneRead returns the pane's visible screen as text.
func (c *Client) PaneRead(ctx context.Context, pane string, lines int) (string, error) {
	out, err := c.run(ctx, DefaultTimeout, "pane", "read", pane, "--source", "visible", "--lines", strconv.Itoa(lines))
	return string(out), err
}

// Agent is herdr's view of the agent in a pane. Kind is empty when the pane
// holds only a shell, for example after the agent exited.
type Agent struct {
	Kind        string `json:"agent"`
	Status      string `json:"agent_status"`
	Name        string `json:"name"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

type agentResult struct {
	Result struct {
		Agent Agent `json:"agent"`
	} `json:"result"`
}

// AgentStart starts an agent in an idle shell pane and waits until it is
// ready for input. args must never contain the prompt.
func (c *Client) AgentStart(ctx context.Context, name, kind, pane string, timeout time.Duration, args []string) (Agent, error) {
	argv := []string{
		"agent", "start", name, "--kind", kind, "--pane", pane,
		"--timeout", strconv.FormatInt(timeout.Milliseconds(), 10), "--",
	}
	argv = append(argv, args...)
	var resp agentResult
	err := c.runJSON(ctx, timeout+5*time.Second, &resp, argv...)
	return resp.Result.Agent, err
}

// AgentPrompt submits text and Enter as one ordered write.
func (c *Client) AgentPrompt(ctx context.Context, target, text string) error {
	return c.runJSON(ctx, DefaultTimeout, nil, "agent", "prompt", target, text)
}

// AgentWait waits for one of the given states (herdr's settled states when
// none are given) and returns the state reached.
func (c *Client) AgentWait(ctx context.Context, target string, timeout time.Duration, until ...string) (string, error) {
	argv := []string{"agent", "wait", target, "--timeout", strconv.FormatInt(timeout.Milliseconds(), 10)}
	for _, u := range until {
		argv = append(argv, "--until", u)
	}
	var resp agentResult
	err := c.runJSON(ctx, timeout+5*time.Second, &resp, argv...)
	return resp.Result.Agent.Status, err
}

// AgentGet returns the agent in target.
func (c *Client) AgentGet(ctx context.Context, target string) (Agent, error) {
	var resp agentResult
	err := c.runJSON(ctx, DefaultTimeout, &resp, "agent", "get", target)
	return resp.Result.Agent, err
}

// AgentList returns every agent herdr knows, keyed by pane id.
func (c *Client) AgentList(ctx context.Context) (map[string]Agent, error) {
	var resp struct {
		Result struct {
			Agents []Agent `json:"agents"`
		} `json:"result"`
	}
	if err := c.runJSON(ctx, DefaultTimeout, &resp, "agent", "list"); err != nil {
		return nil, err
	}
	out := make(map[string]Agent, len(resp.Result.Agents))
	for _, a := range resp.Result.Agents {
		out[a.PaneID] = a
	}
	return out, nil
}

// AgentSendKeys sends herdr logical keys, such as "esc" or "ctrl+c".
func (c *Client) AgentSendKeys(ctx context.Context, target string, keys ...string) error {
	return c.runJSON(ctx, DefaultTimeout, nil, append([]string{"agent", "send-keys", target}, keys...)...)
}

// AgentFocus switches the UI to the agent's pane.
func (c *Client) AgentFocus(ctx context.Context, target string) error {
	return c.runJSON(ctx, DefaultTimeout, nil, "agent", "focus", target)
}
