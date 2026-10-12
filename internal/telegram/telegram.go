package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/state"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// SendTimeout bounds one sendMessage call. There are no retries.
const SendTimeout = 10 * time.Second

// maxText keeps a message well under Telegram's 4096-character limit.
const maxText = 3500

// Client calls the Telegram Bot API.
type Client struct {
	BaseURL string // https://api.telegram.org
	HTTP    *http.Client
}

// New returns a client for the real Bot API.
func New() *Client {
	return &Client{BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: SendTimeout}}
}

// Send posts text to every chat. Errors never contain the token.
func (c *Client) Send(ctx context.Context, creds Creds, text string) error {
	var errs []error
	for _, chat := range creds.ChatIDs {
		if err := c.send(ctx, creds.Token, chat, text); err != nil {
			errs = append(errs, fmt.Errorf("chat %s: %w", chat, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Client) send(ctx context.Context, token, chat, text string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "text": text, "disable_web_page_preview": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return errors.New("build sendMessage request")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// The *url.Error text carries the URL, and the URL carries the token.
		return fmt.Errorf("sendMessage: %s", redact(err.Error(), token))
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		desc := out.Description
		if desc == "" {
			desc = resp.Status
		}
		return fmt.Errorf("sendMessage: %s", redact(desc, token))
	}
	return nil
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}

// Message formats one news event. Every message starts with the task's
// #id tag, which the user's reply repeats to reach that runner.
func Message(ev state.Event, t *task.Task) string {
	tag := "#" + ev.Task
	title := ""
	if t != nil && t.Title != "" {
		title = " · " + t.Title
	}
	var lines []string
	switch ev.Type {
	case "needs-human":
		lines = []string{tag + " needs a decision" + title, ev.Question, "Reply: " + tag + " <answer>"}
	case "held":
		lines = []string{tag + " is waiting for your go" + title, "Reply: " + tag + " ok, or " + tag + " <note> to start it with a note"}
	case "failed":
		lines = []string{tag + " failed" + title, ev.Summary}
	case "stalled":
		lines = []string{tag + " is stalled" + title, ev.Summary, "Check its herdr tab, or reply: " + tag + " <instruction>"}
	case "turn-ended-unreported":
		lines = []string{tag + " stopped without reporting" + title, ev.Summary, "Reply: " + tag + " <instruction>"}
	case "done":
		lines = []string{tag + " done" + title, ev.Summary}
		if len(ev.Outputs) > 0 {
			lines = append(lines, "Output: "+strings.Join(ev.Outputs, ", "))
		}
	default:
		lines = []string{tag + " " + ev.Type + title, ev.Summary}
	}
	var kept []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	text := strings.Join(kept, "\n")
	if r := []rune(text); len(r) > maxText {
		text = string(r[:maxText-1]) + "…"
	}
	return text
}
