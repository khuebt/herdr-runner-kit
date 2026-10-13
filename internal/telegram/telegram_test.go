package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/config"
	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

const token = "123456:SECRET-token"

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveEnvFile(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env.local"), "# bot\nexport TELEGRAM_BOT_TOKEN=\""+token+"\"\nTELEGRAM_CHAT_ID='42, 43'\nOTHER=x\n")
	c, err := Resolve(config.Telegram{EnvFile: "~/.env.local"}, envOf(map[string]string{"HOME": dir}))
	if err != nil || c.Token != token || !slices.Equal(c.ChatIDs, []string{"42", "43"}) {
		t.Fatalf("creds = %+v, %v", c, err)
	}
	c, err = Resolve(config.Telegram{EnvFile: filepath.Join(dir, ".env.local"), ChatID: "7"}, envOf(nil))
	if err != nil || !slices.Equal(c.ChatIDs, []string{"7"}) {
		t.Errorf("chatId override: %+v, %v", c, err)
	}
}

func TestResolvePluginDirOrder(t *testing.T) {
	home, cfgDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	plugin := func(dir, tok string) {
		write(t, filepath.Join(dir, ".env"), "TELEGRAM_BOT_TOKEN="+tok+"\n")
		write(t, filepath.Join(dir, "access.json"), `{"dmPolicy":"allowlist","allowFrom":["111", 222]}`)
	}
	plugin(filepath.Join(home, ".claude", "channels", "telegram"), "home")
	plugin(filepath.Join(cfgDir, "channels", "telegram"), "cfg")
	plugin(stateDir, "state")

	for _, tc := range []struct {
		name string
		cfg  config.Telegram
		env  map[string]string
		want string
	}{
		{"home default", config.Telegram{}, map[string]string{"HOME": home}, "home"},
		{"CLAUDE_CONFIG_DIR", config.Telegram{}, map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": cfgDir}, "cfg"},
		{"TELEGRAM_STATE_DIR", config.Telegram{}, map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": cfgDir, "TELEGRAM_STATE_DIR": stateDir}, "state"},
		{"stateDir key", config.Telegram{StateDir: stateDir}, map[string]string{"HOME": home}, "state"},
	} {
		c, err := Resolve(tc.cfg, envOf(tc.env))
		if err != nil || c.Token != tc.want || !slices.Equal(c.ChatIDs, []string{"111", "222"}) {
			t.Errorf("%s: %+v, %v", tc.name, c, err)
		}
	}
}

func TestResolveMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(config.Telegram{}, envOf(map[string]string{"HOME": dir})); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no plugin: %v", err)
	}
	write(t, filepath.Join(dir, "e"), "TELEGRAM_BOT_TOKEN="+token+"\n")
	if _, err := Resolve(config.Telegram{EnvFile: filepath.Join(dir, "e")}, envOf(nil)); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("no chat: %v", err)
	}
}

func TestSend(t *testing.T) {
	var got map[string]any
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got["chat_id"] == "bad" {
			_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	if err := c.Send(context.Background(), Creds{Token: token, ChatIDs: []string{"42"}}, "hi"); err != nil {
		t.Fatal(err)
	}
	if path != "/bot"+token+"/sendMessage" || got["chat_id"] != "42" || got["text"] != "hi" {
		t.Errorf("request = %s %v", path, got)
	}
	err := c.Send(context.Background(), Creds{Token: token, ChatIDs: []string{"bad"}}, "hi")
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Errorf("err = %v", err)
	}
}

func TestSendErrorNeverShowsToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused
	c := &Client{BaseURL: url, HTTP: &http.Client{}}
	err := c.Send(context.Background(), Creds{Token: token, ChatIDs: []string{"42"}}, "hi")
	if err == nil || strings.Contains(err.Error(), "SECRET") || !strings.Contains(err.Error(), "<token>") {
		t.Errorf("err = %v", err)
	}
}

func TestMessage(t *testing.T) {
	tk := &task.Task{ID: "t3", Title: "fix login"}
	got := Message(state.Event{Task: "t3", Type: "needs-human", Question: "Drop v1? (recommended: yes)"}, tk)
	want := "#t3 needs a decision · fix login\nDrop v1? (recommended: yes)\nReply: #t3 <answer>"
	if got != want {
		t.Errorf("needs-human = %q", got)
	}
	got = Message(state.Event{Task: "t3", Type: "done", Summary: "Fixed.", Outputs: []string{"/o.md"}}, tk)
	if got != "#t3 done · fix login\nFixed.\nOutput: /o.md" {
		t.Errorf("done = %q", got)
	}
	if long := Message(state.Event{Task: "t3", Type: "failed", Summary: strings.Repeat("x", 5000)}, tk); len([]rune(long)) > maxText {
		t.Errorf("message has %d characters", len([]rune(long)))
	}
}

func TestMessageDoneWithGit(t *testing.T) {
	tk := &task.Task{ID: "t3", Title: "fix login"}
	got := Message(state.Event{Task: "t3", Type: "done", Summary: "Fixed.", Git: "on b, 1 commit not pushed"}, tk)
	if got != "#t3 done · fix login\nFixed.\nGit: on b, 1 commit not pushed" {
		t.Errorf("done = %q", got)
	}
	got = Message(state.Event{Task: "t3", Type: "failed", Summary: "Broke.", Git: "repo state, on main, 2 uncommitted"}, tk)
	if got != "#t3 failed · fix login\nBroke.\nGit: repo state, on main, 2 uncommitted" {
		t.Errorf("failed = %q", got)
	}
}

func TestMessageRoundAndDecisions(t *testing.T) {
	tk := &task.Task{ID: "t3", Title: "store"}
	got := Message(state.Event{Task: "t3", Type: "needs-human", Question: "1. Which database? a) Postgres b) SQLite\n2. Which host? a) VPS b) PaaS"}, tk)
	want := "#t3 needs 2 decisions · store\n1. Which database? a) Postgres b) SQLite\n2. Which host? a) VPS b) PaaS\nReply: #t3 1<option> 2<option>, such as 1a 2b"
	if got != want {
		t.Errorf("round = %q", got)
	}
	got = Message(state.Event{Task: "t3", Type: "done", Summary: "Stored.", Decisions: []string{"Cache? -> no", "TTL? -> 1h"}, Git: "on b, clean"}, tk)
	want = "#t3 done · store\nDecided: Cache? -> no\nDecided: TTL? -> 1h\nStored.\nGit: on b, clean"
	if got != want {
		t.Errorf("done = %q", got)
	}
}
