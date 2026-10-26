package herdr

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func fakeExec(stdout, stderr string, err error) ExecFunc {
	return func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte(stdout), []byte(stderr), err
	}
}

func TestErrorCodeIsParsed(t *testing.T) {
	c := &Client{Bin: "herdr", Exec: fakeExec("", `{"error":{"code":"agent_not_ready","message":"blocked during startup"},"id":"cli:agent:start"}`, errors.New("exit status 1"))}
	_, err := c.AgentStart(context.Background(), "a", "claude", "w1:p1", time.Second, nil)
	if !HasCode(err, CodeAgentNotReady) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "herdr agent start: agent_not_ready: blocked during startup") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestPlainStderrBecomesMessage(t *testing.T) {
	c := &Client{Bin: "herdr", Exec: fakeExec("", "usage: herdr tab close <tab_id>\n", errors.New("exit status 2"))}
	err := c.TabClose(context.Background(), "")
	if err == nil || HasCode(err, CodeTimeout) || !strings.Contains(err.Error(), "usage: herdr tab close") {
		t.Fatalf("err = %v", err)
	}
}

func TestTabCreateParsesIDs(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{"result":{"root_pane":{"pane_id":"w1:p3","tab_id":"w1:t2","workspace_id":"w1"},"tab":{}}}`), nil, nil
	}}
	loc, err := c.TabCreate(context.Background(), "w1", "/repo", "t1 · x", []string{"HRK_TASK=t1"})
	if err != nil || loc != (Location{WorkspaceID: "w1", TabID: "w1:t2", PaneID: "w1:p3"}) {
		t.Fatalf("loc = %+v, %v", loc, err)
	}
	want := "tab create --workspace w1 --cwd /repo --label t1 · x --no-focus --env HRK_TASK=t1"
	if strings.Join(got, " ") != want {
		t.Errorf("argv = %q", strings.Join(got, " "))
	}
}

func TestWorkspaceCreateParsesIDs(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{"result":{"type":"workspace_created","workspace":{"workspace_id":"w5"},"tab":{"tab_id":"w5:t1"},"root_pane":{"pane_id":"w5:p1","tab_id":"w5:t1","workspace_id":"w5"}}}`), nil, nil
	}}
	loc, err := c.WorkspaceCreate(context.Background(), "/repo", "demo-app")
	if err != nil || loc != (Location{WorkspaceID: "w5", TabID: "w5:t1", PaneID: "w5:p1"}) {
		t.Fatalf("loc = %+v, %v", loc, err)
	}
	if want := "workspace create --cwd /repo --label demo-app --no-focus"; strings.Join(got, " ") != want {
		t.Errorf("argv = %q", strings.Join(got, " "))
	}

	c.Exec = fakeExec("", `{"error":{"code":"invalid_cwd","message":"no such folder"}}`, errors.New("exit status 1"))
	if _, err := c.WorkspaceCreate(context.Background(), "/gone", "demo-app"); !HasCode(err, "invalid_cwd") {
		t.Errorf("failed create: %v", err)
	}
}

func TestUnexpectedOutputIsAnError(t *testing.T) {
	c := &Client{Bin: "herdr", Exec: fakeExec("not json", "", nil)}
	if _, err := c.AgentGet(context.Background(), "a"); err == nil {
		t.Fatal("want error for non-JSON output")
	}
}

func TestWorkspaceCheckout(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{"result":{"type":"workspace_info","workspace":{"workspace_id":"w2","worktree":{"checkout_path":"/wt/t1","is_linked_worktree":true,"repo_root":"/repo"}}}}`), nil, nil
	}}
	path, err := c.WorkspaceCheckout(context.Background(), "w2")
	if err != nil || path != "/wt/t1" {
		t.Fatalf("path = %q, %v", path, err)
	}
	if strings.Join(got, " ") != "workspace get w2" {
		t.Errorf("argv = %q", got)
	}

	c.Exec = fakeExec(`{"result":{"workspace":{"workspace_id":"w3"}}}`, "", nil)
	if path, err := c.WorkspaceCheckout(context.Background(), "w3"); err != nil || path != "" {
		t.Errorf("non-git workspace: %q, %v", path, err)
	}
	c.Exec = fakeExec("", `{"error":{"code":"workspace_not_found","message":"no such workspace"}}`, errors.New("exit status 1"))
	if _, err := c.WorkspaceCheckout(context.Background(), "w4"); !HasCode(err, "workspace_not_found") {
		t.Errorf("closed workspace: %v", err)
	}
}

func TestWorkspaceLabel(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{"result":{"workspace":{"workspace_id":"w2","label":"demo-app","tab_count":1}}}`), nil, nil
	}}
	label, found, err := c.WorkspaceLabel(context.Background(), "w2")
	if err != nil || !found || label != "demo-app" || strings.Join(got, " ") != "workspace get w2" {
		t.Fatalf("label %q found %v err %v argv %q", label, found, err, got)
	}
	c.Exec = fakeExec("", `{"error":{"code":"workspace_not_found","message":"no such workspace"}}`, errors.New("exit status 1"))
	if _, found, err := c.WorkspaceLabel(context.Background(), "w4"); err != nil || found {
		t.Errorf("closed workspace: found %v, %v", found, err)
	}
	c.Exec = fakeExec("", "herdr is not running", errors.New("exit status 1"))
	if _, _, err := c.WorkspaceLabel(context.Background(), "w4"); err == nil {
		t.Error("a herdr failure was taken for a closed workspace")
	}
}

func TestWorkspaceClose(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{}`), nil, nil
	}}
	if err := c.WorkspaceClose(context.Background(), "w2"); err != nil || strings.Join(got, " ") != "workspace close w2" {
		t.Fatalf("err %v argv %q", err, got)
	}
	c.Exec = fakeExec("", `{"error":{"code":"workspace_not_found","message":"no such workspace"}}`, errors.New("exit status 1"))
	if err := c.WorkspaceClose(context.Background(), "w4"); !HasCode(err, CodeNoWorkspace) {
		t.Errorf("closed workspace: %v", err)
	}
}

func TestWorkspaceRename(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{}`), nil, nil
	}}
	if err := c.WorkspaceRename(context.Background(), "w2", "t1 · done · worktree kept"); err != nil ||
		strings.Join(got, "|") != "workspace|rename|w2|t1 · done · worktree kept" {
		t.Fatalf("err %v argv %q", err, got)
	}
}

func TestRepoWorkspaceSkipsLinkedWorktrees(t *testing.T) {
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		switch strings.Join(args, " ") {
		case "workspace list":
			return []byte(`{"result":{"workspaces":[{"workspace_id":"w1"},{"workspace_id":"w2"}]}}`), nil, nil
		case "workspace get w1":
			return []byte(`{"result":{"workspace":{"worktree":{"checkout_path":"/wt/a","is_linked_worktree":true,"repo_root":"/repo"}}}}`), nil, nil
		default:
			return []byte(`{"result":{"workspace":{"worktree":{"checkout_path":"/repo","is_linked_worktree":false,"repo_root":"/repo"}}}}`), nil, nil
		}
	}}
	if ws, err := c.RepoWorkspace(context.Background(), "/repo"); err != nil || ws != "w2" {
		t.Errorf("RepoWorkspace = %q, %v", ws, err)
	}
}

func TestPaneProcessInfo(t *testing.T) {
	var got []string
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(`{"id":"cli:pane:process_info","result":{"process_info":{"foreground_process_group_id":12,"foreground_processes":[{"pid":12,"name":"claude"}],"pane_id":"w1:p1","shell_pid":11},"type":"pane_process_info"}}`), nil, nil
	}}
	info, err := c.PaneProcessInfo(context.Background(), "w1:p1")
	if err != nil || info.ShellPID != 11 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if strings.Join(got, " ") != "pane process-info --pane w1:p1" {
		t.Errorf("argv = %q", strings.Join(got, " "))
	}
}

func TestPaneClaudeSession(t *testing.T) {
	var got []string
	out := `{"id":"cli:pane:get","result":{"pane":{"agent":"claude","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"abc-123"},"pane_id":"w1:p2"},"type":"pane_info"}}`
	c := &Client{Bin: "herdr", Exec: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		got = args
		return []byte(out), nil, nil
	}}
	if id, err := c.PaneClaudeSession(context.Background(), "w1:p2"); err != nil || id != "abc-123" {
		t.Fatalf("id = %q, %v", id, err)
	}
	if strings.Join(got, " ") != "pane get w1:p2" {
		t.Errorf("argv = %q", strings.Join(got, " "))
	}
	for _, o := range []string{
		`{"result":{"pane":{"pane_id":"w1:p2"}}}`,
		`{"result":{"pane":{"agent_session":{"agent":"codex","kind":"id","value":"x"}}}}`,
	} {
		out = o
		if id, err := c.PaneClaudeSession(context.Background(), "w1:p2"); err != nil || id != "" {
			t.Errorf("%s: id = %q, %v", o, id, err)
		}
	}
}
