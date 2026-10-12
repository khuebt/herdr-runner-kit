package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tarball(t *testing.T, bin []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range map[string][]byte{"README.md": []byte("readme"), binaryName: bin} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeGitHub serves one release, v0.2.0, over TLS. tamper corrupts its
// checksum. calls counts API requests.
func fakeGitHub(t *testing.T, tamper bool) (*Client, *atomic.Int32) {
	t.Helper()
	archive := tarball(t, []byte("new binary"))
	sum := sha256.Sum256(archive)
	if tamper {
		sum[0] ^= 0xff
	}
	name := ArchiveName("v0.2.0")
	sums := hex.EncodeToString(sum[:]) + "  " + name + "\n"
	var calls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := Release{Tag: "v0.2.0", Body: "notes 0.2.0", Assets: []Asset{
			{Name: name, URL: srv.URL + "/dl/archive"},
			{Name: "checksums.txt", URL: srv.URL + "/dl/sums"},
		}}
		switch r.URL.Path {
		case "/repos/" + Repo + "/releases/latest", "/repos/" + Repo + "/releases/tags/v0.2.0":
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(rel)
		case "/dl/archive":
			_, _ = w.Write(archive)
		case "/dl/sums":
			_, _ = w.Write([]byte(sums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{API: srv.URL, HTTP: srv.Client()}, &calls
}

func installed(t *testing.T) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), binaryName)
	if err := os.WriteFile(dest, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dest
}

func content(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestInstallReplacesTheBinary(t *testing.T) {
	c, _ := fakeGitHub(t, false)
	rel, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dest := installed(t)
	var seen string
	if err := c.Install(context.Background(), rel, dest, func(staged string) error {
		seen = content(t, staged)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := content(t, dest); got != "new binary" || seen != "new binary" {
		t.Fatalf("dest %q, staged %q", got, seen)
	}
	if info, _ := os.Stat(dest); info.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", info.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".*")); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}

func TestInstallKeepsTheOldBinary(t *testing.T) {
	ctx := context.Background()
	c, _ := fakeGitHub(t, true)
	rel, err := c.Tag(ctx, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	dest := installed(t)
	if err := c.Install(ctx, rel, dest, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered: %v", err)
	}
	if got := content(t, dest); got != "old binary" {
		t.Fatalf("dest changed to %q", got)
	}

	// A veto from the check also leaves the old binary, and no staged file.
	c, _ = fakeGitHub(t, false)
	if rel, err = c.Tag(ctx, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	veto := errors.New("veto")
	if err := c.Install(ctx, rel, dest, func(string) error { return veto }); !errors.Is(err, veto) {
		t.Fatalf("veto: %v", err)
	}
	if got := content(t, dest); got != "old binary" {
		t.Fatalf("dest changed to %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(dest), ".*")); len(left) != 0 {
		t.Errorf("leftovers: %v", left)
	}
}

func TestPlainHTTPIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	c := &Client{API: srv.URL, HTTP: srv.Client()}
	if _, err := c.Latest(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("got %v", err)
	}
}

func TestLatestCachedAsksOncePerDay(t *testing.T) {
	c, calls := fakeGitHub(t, false)
	cache := filepath.Join(t.TempDir(), "latest.json")
	for range 3 {
		tag, err := c.LatestCached(context.Background(), cache, 24*time.Hour)
		if err != nil || tag != "v0.2.0" {
			t.Fatalf("tag %q, %v", tag, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d API calls, want 1", n)
	}
	if _, err := c.LatestCached(context.Background(), cache, 0); err != nil || calls.Load() != 2 {
		t.Fatalf("maxAge 0 must ask again: %d calls, %v", calls.Load(), err)
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"v0.1.0", "v0.2.0", -1},
		{"v1.0.0", "v0.9.9", 1},
		{"v0.10.0", "v0.9.0", 1},
		{"v1.0.0-rc.1", "v1.0.0", -1},
		{"v1.0.0-rc.2", "v1.0.0-rc.10", -1},
		{"dev", "v0.1.0", -1},
		{"v0.1.0", "dev", 1},
	} {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
	if Major("v2.3.4") != 2 || Major("dev") != -1 {
		t.Error("Major")
	}
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"v0.1.0", "v0.1.5", false},
		{"v0.1.0", "v0.2.0", true},
		{"v0.9.0", "v1.0.0", true},
		{"v1.2.0", "v1.3.0", false},
		{"v1.2.0", "v2.0.0", true},
		{"dev", "v0.2.0", false},
	} {
		if got := MayBreak(tc.from, tc.to); got != tc.want {
			t.Errorf("MayBreak(%s, %s) = %v", tc.from, tc.to, got)
		}
	}
}

func TestBetween(t *testing.T) {
	rs := []Release{{Tag: "v0.4.0"}, {Tag: "v0.3.0-rc.1", Prerelease: true}, {Tag: "v0.3.0"}, {Tag: "v0.2.0"}, {Tag: "v0.1.0"}}
	var tags []string
	for _, r := range Between(rs, "v0.1.0", "v0.3.0") {
		tags = append(tags, r.Tag)
	}
	if strings.Join(tags, " ") != "v0.2.0 v0.3.0" {
		t.Fatalf("got %v", tags)
	}
}
