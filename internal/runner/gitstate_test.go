package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseStatus(t *testing.T) {
	z := func(fields ...string) string { return strings.Join(fields, "\x00") + "\x00" }
	tests := []struct {
		name string
		out  string
		want gitStatus
	}{
		{
			name: "detached head",
			out:  z("# branch.oid 1234", "# branch.head (detached)"),
			want: gitStatus{Head: "(detached)"},
		},
		{
			name: "no upstream",
			out:  z("# branch.oid 1234", "# branch.head hrk/t1-x", "? notes.txt"),
			want: gitStatus{Head: "hrk/t1-x", Untracked: []string{"notes.txt"}},
		},
		{
			name: "diverged",
			out:  z("# branch.head main", "# branch.upstream origin/main", "# branch.ab +2 -3"),
			want: gitStatus{Head: "main", Upstream: "origin/main", Ahead: 2, Behind: 3},
		},
		{
			name: "changes, rename, conflict, untracked and ignored",
			out: z("# branch.head main",
				"1 .M N... 100644 100644 100644 abc abc a b.txt",
				"2 R. N... 100644 100644 100644 abc abc R100 new name.txt", "old name.txt",
				"u UU N... 100644 100644 100644 100644 abc abc abc c.txt",
				"? plans/x.md", "? d e.txt",
				"! build/", "! x.log"),
			want: gitStatus{
				Head:      "main",
				Changed:   []string{"a b.txt", "new name.txt", "c.txt"},
				Untracked: []string{"plans/x.md", "d e.txt"},
				Ignored:   2,
			},
		},
	}
	for _, tt := range tests {
		got, err := parseStatus(tt.out)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if got.Head != tt.want.Head || got.Upstream != tt.want.Upstream || got.Ahead != tt.want.Ahead ||
			got.Behind != tt.want.Behind || got.Ignored != tt.want.Ignored ||
			!slices.Equal(got.Changed, tt.want.Changed) || !slices.Equal(got.Untracked, tt.want.Untracked) {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
	for _, bad := range []string{z("# branch.ab 2 3"), z("X what")} {
		if _, err := parseStatus(bad); err == nil {
			t.Errorf("parseStatus(%q) succeeded", bad)
		}
	}
}

func TestReadStatusListsUntrackedFiles(t *testing.T) {
	repo := gitRepo(t)
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("*.log\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-q", "-m", "ignore logs")
	for _, f := range []string{"plans/a/b.md", "x.log", "notes.txt"} {
		writeFile(t, filepath.Join(repo, f))
	}
	st, err := readStatus(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(st.Untracked)
	if st.Head != "main" || len(st.Changed) != 0 || st.Ignored != 1 ||
		!slices.Equal(st.Untracked, []string{"notes.txt", "plans/a/b.md"}) {
		t.Errorf("status = %+v", st)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
