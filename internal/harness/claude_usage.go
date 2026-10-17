package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// Usage reads Claude Code transcripts; see ClaudeUsage.
func (claude) Usage(transcripts []string, from, to time.Time) (*task.Usage, error) {
	return ClaudeUsage(transcripts, from, to)
}

// ClaudeUsage sums the model calls in Claude Code session transcripts and in
// the subagent transcripts Claude keeps beside each one, in
// <session>/subagents/agent-*.jsonl. A transcript holds one JSON row per
// line; a call is an assistant row with usage, and Claude writes one row per
// content block of a call, all with the call's message id, so rows of one id
// count once with the largest of each field. Rows of the synthetic model,
// rows of other types and lines that do not parse are skipped. Calls outside
// from and to (zero leaves a side open) are not counted. It returns nil and
// the first error when no transcript could be read.
func ClaudeUsage(transcripts []string, from, to time.Time) (*task.Usage, error) {
	var u task.Usage
	var firstErr error
	read := 0
	for _, path := range transcripts {
		if err := claudeFileUsage(&u, path, from, to, false); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		read++
		subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "agent-*.jsonl"))
		for _, sub := range subs {
			calls := u.Calls
			if claudeFileUsage(&u, sub, from, to, true) == nil && u.Calls > calls {
				u.Subagents++
			}
		}
	}
	if read == 0 && firstErr != nil {
		return nil, firstErr
	}
	u.At = time.Now().UTC()
	return &u, nil
}

// claudeSkipped starts the rows that are large and carry no usage: request
// blobs and file snapshots.
var claudeSkipped = [][]byte{[]byte(`{"type":"api-request`), []byte(`{"type":"file-history`)}

// claudeCall is one model call's token counts.
type claudeCall struct{ input, cacheRead, cacheWrite, cacheWrite1h, output int64 }

// claudeFileUsage adds the calls in one transcript to u.
func claudeFileUsage(u *task.Usage, path string, from, to time.Time, subagent bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var calls []claudeCall
	byID := map[string]int{}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if c, id, ok := claudeRowCall(line, from, to); ok {
				if i, seen := byID[id]; seen && id != "" {
					p := &calls[i]
					p.input, p.cacheRead = max(p.input, c.input), max(p.cacheRead, c.cacheRead)
					p.cacheWrite, p.cacheWrite1h = max(p.cacheWrite, c.cacheWrite), max(p.cacheWrite1h, c.cacheWrite1h)
					p.output = max(p.output, c.output)
				} else {
					byID[id] = len(calls)
					calls = append(calls, c)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	for _, c := range calls {
		u.AddCall(c.input, c.cacheRead, c.cacheWrite, c.cacheWrite1h, c.output, subagent)
	}
	return nil
}

// claudeRowCall reads one transcript line, and ok says it is a call to
// count.
func claudeRowCall(line []byte, from, to time.Time) (claudeCall, string, bool) {
	for _, p := range claudeSkipped {
		if bytes.HasPrefix(line, p) {
			return claudeCall{}, "", false
		}
	}
	if !bytes.Contains(line, []byte(`"assistant"`)) {
		return claudeCall{}, "", false
	}
	var row struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input         int64 `json:"input_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
				Creation      struct {
					OneHour int64 `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &row) != nil || row.Type != "assistant" || row.Message.Usage == nil || row.Message.Model == "<synthetic>" {
		return claudeCall{}, "", false
	}
	if !from.IsZero() || !to.IsZero() {
		at, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil || (!from.IsZero() && at.Before(from)) || (!to.IsZero() && at.After(to)) {
			return claudeCall{}, "", false
		}
	}
	us := row.Message.Usage
	c := claudeCall{us.Input, us.CacheRead, us.CacheCreation, us.Creation.OneHour, us.Output}
	return c, row.Message.ID, c != claudeCall{}
}
