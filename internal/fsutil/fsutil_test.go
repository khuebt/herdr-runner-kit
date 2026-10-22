package fsutil

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A second Lock on the same path waits for the first to be released, on a
// lock file and on a folder alike, and a folder lock leaves no file behind.
func TestLock(t *testing.T) {
	dir := t.TempDir()
	for _, path := range []string{filepath.Join(dir, "x.lock"), dir} {
		unlock, err := Lock(path)
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan struct{})
		go func() {
			unlock2, err := Lock(path)
			if err != nil {
				t.Error(err)
			} else {
				unlock2()
			}
			close(got)
		}()
		select {
		case <-got:
			t.Fatalf("%s: second lock taken while the first is held", path)
		case <-time.After(50 * time.Millisecond):
		}
		unlock()
		<-got
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || entries[0].Name() != "x.lock" {
		t.Errorf("folder holds %v, want only x.lock", entries)
	}
}

// Writers that update one file together never lose each other's change.
func TestUpdateConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list")
	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := Update(path, dir, 0o640, func(old []byte) ([]byte, error) {
				return fmt.Appendf(old, "w%d\n", i), nil
			}, nil)
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, _ := os.ReadFile(path)
	for i := range n {
		if !bytes.Contains(data, fmt.Appendf(nil, "w%d\n", i)) {
			t.Errorf("w%d lost:\n%s", i, data)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	// An unchanged result writes nothing.
	before, _ := os.Stat(path)
	if err := Update(path, dir, 0o600, func(old []byte) ([]byte, error) { return old, nil }, nil); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Error("an unchanged edit replaced the file")
	}
}

// A change another program writes over is written again, and a change it
// writes over every time is an error that names the file.
func TestUpdateRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg")
	if err := os.WriteFile(path, []byte("user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overwrites := 1
	UpdateWritten = func(p string) {
		if overwrites > 0 {
			overwrites--
			if err := WriteFileAtomic(p, []byte("user\n"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { UpdateWritten = func(string) {} })
	edits := 0
	add := func(old []byte) ([]byte, error) {
		edits++
		if bytes.Contains(old, []byte("ours\n")) {
			return old, nil
		}
		return append(old, "ours\n"...), nil
	}
	landed := func(b []byte) bool { return bytes.Contains(b, []byte("ours\n")) }
	if err := Update(path, dir, 0o600, add, landed); err != nil || edits != 2 {
		t.Fatalf("one overwrite: edits = %d, err = %v", edits, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "user\nours\n" {
		t.Fatalf("file = %q", data)
	}

	if err := os.WriteFile(path, []byte("user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overwrites, edits = 100, 0
	err := Update(path, dir, 0o600, add, landed)
	if err == nil || !strings.Contains(err.Error(), path) || edits != 3 {
		t.Fatalf("always overwritten: edits = %d, err = %v", edits, err)
	}
}

// Writing through a symlink keeps the link and changes its target.
func TestWriteFileAtomicFollowsLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink("dotfiles/settings.json", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dotfiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(link, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link must stay a link")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "dotfiles", "settings.json")); string(data) != "{}" {
		t.Fatalf("target = %q", data)
	}
}

// Of two creators of one file, exactly one creates it, and the file is
// never seen partly written.
func TestCreateExclusive(t *testing.T) {
	for range 20 {
		path := filepath.Join(t.TempDir(), "access.json")
		datas := [][]byte{bytes.Repeat([]byte("a"), 1<<16), bytes.Repeat([]byte("b"), 1<<16)}
		created := make([]bool, len(datas))
		var wg sync.WaitGroup
		for i, data := range datas {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ok, err := CreateExclusive(path, data, 0o600)
				if err != nil {
					t.Error(err)
				}
				created[i] = ok
			}()
		}
		wg.Wait()
		if created[0] == created[1] {
			t.Fatalf("created = %v, want exactly one", created)
		}
		winner := datas[0]
		if created[1] {
			winner = datas[1]
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, winner) {
			t.Fatal("the file is not the winner's whole content")
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 1 {
			t.Fatalf("temporary files left: %v", entries)
		}
	}
}
