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

func TestUnexpectedOutputIsAnError(t *testing.T) {
	c := &Client{Bin: "herdr", Exec: fakeExec("not json", "", nil)}
	if _, err := c.AgentGet(context.Background(), "a"); err == nil {
		t.Fatal("want error for non-JSON output")
	}
}
