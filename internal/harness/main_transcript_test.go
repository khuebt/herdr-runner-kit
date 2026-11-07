package harness

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func TestMainTranscript(t *testing.T) {
	for _, h := range []task.Harness{task.Pi, task.Omp} {
		t.Run(string(h), func(t *testing.T) {
			data, err := os.ReadFile("testdata/main-news-" + string(h) + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			target := MainTranscriptTarget{Harness: h, ID: "session-main-news", File: filepath.Join(t.TempDir(), "session.jsonl"), Cwd: "/probe"}
			receipt := &MainTranscriptReceipt{BatchID: "fresh-trigger-turn", Owner: "/probe", Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(`["Local news fresh-trigger-turn"]`))), Content: "Local news fresh-trigger-turn"}
			for _, tc := range []struct {
				name, data string
				found, bad bool
			}{
				{"valid", string(data), true, false},
				{"compacted", string(data) + `{"type":"compaction","summary":"news summarized","firstKeptEntryId":"another-branch"}` + "\n", true, false},
				{"absent", strings.ReplaceAll(string(data), "fresh-trigger-turn", "another-batch"), false, false},
				{"user-text", `{"type":"session","version":3,"id":"session-main-news","cwd":"/probe"}` + "\n" + `{"type":"message","message":{"role":"user","content":"fresh-trigger-turn hrk-news"}}` + "\n", false, false},
				{"wrong-content", strings.ReplaceAll(string(data), "Local news fresh-trigger-turn", "altered"), false, true},
				{"wrong-owner", strings.ReplaceAll(string(data), `"owner":"/probe"`, `"owner":"/other"`), false, true},
				{"wrong-digest", strings.ReplaceAll(string(data), receipt.Digest, "bad"), false, true},
				{"wrong-session", strings.ReplaceAll(string(data), "session-main-news", "other-session"), false, true},
				{"wrong-cwd", strings.ReplaceAll(string(data), `"cwd":"/probe"`, `"cwd":"/other"`), false, true},
				{"partial-tail", string(data) + `{"type":"message"`, false, true},
				{"missing-newline", strings.TrimSuffix(string(data), "\n"), false, true},
				{"corrupt-interior", "{broken}\n" + string(data), false, true},
				{"invalid-utf8", string(data) + "{\"type\":\"message\",\"content\":\"\xff\"}\n", false, true},
				{"duplicate", string(data) + string(data), false, true},
				{"empty", "", false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := os.WriteFile(target.File, []byte(tc.data), 0o600); err != nil {
						t.Fatal(err)
					}
					found, err := VerifyMainTranscript(target, receipt)
					if found != tc.found || (err != nil) != tc.bad {
						t.Fatalf("found=%v err=%v", found, err)
					}
				})
			}
			if err := os.WriteFile(target.File, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyMainTranscript(target, nil); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("sync failed")
			if found, err := verifyMainTranscript(target, receipt, func(*os.File) error { return failure }); found || !errors.Is(err, failure) {
				t.Fatalf("sync failure: %v %v", found, err)
			}
			if _, err := verifyMainTranscript(target, receipt, func(*os.File) error { return os.Truncate(target.File, 0) }); err == nil {
				t.Fatal("accepted concurrent truncation")
			}
			if os.Geteuid() != 0 {
				if err := os.Chmod(target.File, 0); err != nil {
					t.Fatal(err)
				}
				if found, err := VerifyMainTranscript(target, receipt); found || !errors.Is(err, os.ErrPermission) {
					t.Fatalf("permission failure: %v %v", found, err)
				}
				if err := os.Chmod(target.File, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Remove(target.File); err != nil {
				t.Fatal(err)
			}
			if found, err := VerifyMainTranscript(target, receipt); found || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("missing: %v %v", found, err)
			}
			if err := os.Mkdir(target.File, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyMainTranscript(target, receipt); err == nil {
				t.Fatal("accepted directory")
			}
		})
	}
}

func TestMainTranscriptRejectsUnsupportedIdentity(t *testing.T) {
	for _, h := range []task.Harness{task.Claude, task.Codex, "unknown"} {
		if _, err := VerifyMainTranscript(MainTranscriptTarget{Harness: h, ID: "id", File: "/tmp/transcript", Cwd: "/tmp"}, nil); err == nil {
			t.Fatal("unsupported harness accepted")
		}
	}
}
