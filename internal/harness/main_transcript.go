package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
	"github.com/khuebt/herdr-runner-kit/internal/task"
)

// MainTranscriptTarget is the identity supplied by the runtime session manager.
// Neither runtime's header names its harness; dispatch uses the registered harness.
type MainTranscriptTarget struct {
	Harness       task.Harness
	ID, File, Cwd string
}

// MainTranscriptReceipt contains the expected immutable payload, not client proof.
type MainTranscriptReceipt struct {
	BatchID string `json:"batchId"`
	Owner   string `json:"owner"`
	Digest  string `json:"digest"`
	Content string `json:"-"`
}

// VerifyMainTranscript checks the entire physical history, including inactive
// branches and compacted entries. A nil receipt verifies only the initial header.
// It syncs the runtime's file and directory without writing either. Missing or
// incomplete files are errors; only a valid complete file can prove absence.
func VerifyMainTranscript(target MainTranscriptTarget, receipt *MainTranscriptReceipt) (bool, error) {
	return verifyMainTranscript(target, receipt, func(f *os.File) error {
		if err := f.Sync(); err != nil {
			return err
		}
		return fsutil.SyncDir(filepath.Dir(target.File))
	})
}

func verifyMainTranscript(target MainTranscriptTarget, receipt *MainTranscriptReceipt, syncFile func(*os.File) error) (bool, error) {
	if (target.Harness != task.Pi && target.Harness != task.Omp) || target.ID == "" || !filepath.IsAbs(target.File) || filepath.Clean(target.File) != target.File || !filepath.IsAbs(target.Cwd) || filepath.Clean(target.Cwd) != target.Cwd {
		return false, fmt.Errorf("invalid main transcript identity")
	}
	if receipt != nil && (receipt.BatchID == "" || receipt.Owner == "" || receipt.Digest == "" || receipt.Content == "") {
		return false, fmt.Errorf("incomplete main transcript receipt")
	}
	f, err := os.Open(target.File)
	if err != nil {
		return false, fmt.Errorf("open registered transcript: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("transcript must be a regular file")
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	headers, matches := 0, 0
	for line := 1; ; line++ {
		var data []byte
		for {
			part, readErr := reader.ReadSlice('\n')
			if len(data)+len(part) > 4<<20 {
				return false, fmt.Errorf("transcript line %d exceeds limit", line)
			}
			data = append(data, part...)
			if readErr == bufio.ErrBufferFull {
				continue
			}
			if readErr == io.EOF && len(data) == 0 {
				break
			}
			if readErr != nil {
				return false, fmt.Errorf("incomplete transcript line %d: %w", line, readErr)
			}
			break
		}
		if len(data) == 0 {
			break
		}
		var entry struct {
			Type       string          `json:"type"`
			Version    int             `json:"version"`
			ID         string          `json:"id"`
			Cwd        string          `json:"cwd"`
			CustomType string          `json:"customType"`
			Content    json.RawMessage `json:"content"`
			Details    json.RawMessage `json:"details"`
		}
		if !utf8.Valid(data) {
			return false, fmt.Errorf("invalid transcript UTF-8 at line %d", line)
		}
		if err := json.Unmarshal(data, &entry); err != nil || entry.Type == "" {
			return false, fmt.Errorf("invalid transcript JSON at line %d", line)
		}
		if entry.Type == "session" {
			headers++
			if headers != 1 || entry.Version != 3 || entry.ID != target.ID || entry.Cwd != target.Cwd {
				return false, fmt.Errorf("transcript header identity or version mismatch")
			}
		}
		if receipt == nil || entry.Type != "custom_message" || entry.CustomType != "hrk-news" {
			continue
		}
		var details MainTranscriptReceipt
		if err := json.Unmarshal(entry.Details, &details); err != nil {
			return false, fmt.Errorf("invalid news receipt details")
		}
		if details.BatchID != receipt.BatchID {
			continue
		}
		var content string
		if err := json.Unmarshal(entry.Content, &content); err != nil || content != receipt.Content || details.Owner != receipt.Owner || details.Digest != receipt.Digest || entry.ID == "" {
			return false, fmt.Errorf("transcript news payload mismatch")
		}
		matches++
		if matches > 1 {
			return false, fmt.Errorf("duplicate transcript news receipt")
		}
	}
	if headers != 1 {
		return false, fmt.Errorf("transcript header missing; initialize the runtime session before registering news")
	}
	if err := syncFile(f); err != nil {
		return false, fmt.Errorf("sync transcript receipt: %w", err)
	}
	current, err := os.Stat(target.File)
	if err != nil {
		return false, err
	}
	if !os.SameFile(info, current) || current.Size() < info.Size() {
		return false, fmt.Errorf("transcript replaced or truncated during verification")
	}
	return matches == 1, nil
}
