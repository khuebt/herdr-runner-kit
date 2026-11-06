package state

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/khuebt/herdr-runner-kit/internal/task"
)

func testMainNews() MainNews {
	sum := sha256.Sum256(nil)
	target := MainSession{Owner: "/repo", Harness: task.Pi, ID: "session", Transcript: "/repo/session.jsonl", Cwd: "/repo", UpdatedAt: time.Now().UTC()}
	return MainNews{
		Version: MainNewsVersion, Owner: "/repo", Cursor: CursorName("/repo"), Generation: 1, ActiveSince: time.Now().UTC(),
		Lease:   &MainNewsLease{PID: 123, ProcessStart: "start", Nonce: "nonce"},
		Log:     MainNewsLog{Identity: "1:2", Offset: 50, Digest: hex.EncodeToString(sum[:])},
		Pending: &MainNewsBatch{BatchID: "batch", From: 0, To: 50, Lines: []string{"first", "second"}, Digest: NewsDigest([]string{"first", "second"}), CreatedAt: time.Now().UTC(), Target: target, Generation: 1, Attempt: "prepared"},
	}
}

func TestMainNewsRoundTripAndValidation(t *testing.T) {
	s := openStore(t)
	if n, err := s.ReadMainNews("/repo"); err != nil || n != nil {
		t.Fatalf("missing: %+v %v", n, err)
	}
	n := testMainNews()
	if err := s.SaveMainNews(n); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadMainNews(n.Owner)
	if err != nil || !reflect.DeepEqual(*got, n) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	info, err := os.Stat(s.mainNewsPath(n.Owner))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", info, err)
	}
	before, err := os.ReadFile(s.mainNewsPath(n.Owner))
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*MainNews){
		"version":    func(n *MainNews) { n.Version++ },
		"cursor":     func(n *MainNews) { n.Cursor = "tasks-other" },
		"lease":      func(n *MainNews) { n.Lease.Nonce = "" },
		"offset":     func(n *MainNews) { n.Pending.From = -1 },
		"range":      func(n *MainNews) { n.Pending.To = n.Pending.From },
		"lines":      func(n *MainNews) { n.Pending.Lines[0] = "changed" },
		"target":     func(n *MainNews) { n.Pending.Target.Owner = "/other" },
		"relative":   func(n *MainNews) { n.Pending.Target.Transcript = "relative" },
		"generation": func(n *MainNews) { n.Pending.Generation++ },
		"attempt":    func(n *MainNews) { n.Pending.Attempt = "committed" },
		"log":        func(n *MainNews) { n.Log.Offset++ },
		"digest":     func(n *MainNews) { n.Log.Digest = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			n := testMainNews()
			mutate(&n)
			if err := s.SaveMainNews(n); err == nil {
				t.Fatal("invalid state accepted")
			}
			after, err := os.ReadFile(s.mainNewsPath("/repo"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("invalid save changed pending: %v", err)
			}
		})
	}
}

func TestMainNewsCorruptionFailsClosed(t *testing.T) {
	for _, data := range []string{"", `{`, `{"version":2}`, `{"version":1,"owner":"/other"}`} {
		s := openStore(t)
		path := s.mainNewsPath("/repo")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadMainNews("/repo"); err == nil {
			t.Fatalf("accepted corruption %q", data)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != data {
			t.Fatal("corrupt file was discarded")
		}
	}
}

func TestNewsDigestPreservesBoundaries(t *testing.T) {
	if NewsDigest([]string{"a", "b"}) == NewsDigest([]string{"a\nb"}) {
		t.Fatal("digest collapsed line boundaries")
	}
	if NewsDigest([]string{"a "}) == NewsDigest([]string{"a"}) {
		t.Fatal("digest discarded whitespace")
	}
}
