package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// binaryName is the executable inside each release archive. Releases from
// before the rename hold it only as oldBinaryName, which also names every
// archive.
const (
	binaryName    = "hrk"
	oldBinaryName = "herdr-runner-kit"
)

// ArchiveName is the archive goreleaser builds for this platform.
func ArchiveName(tag string) string {
	return fmt.Sprintf("%s_%s_%s_%s.tar.gz", oldBinaryName, strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
}

// Install downloads rel's archive for this platform, verifies it against the
// release's checksums.txt, and puts its binary at dest. The new binary is
// written beside dest and renamed over it, so dest is never half written and
// a running copy keeps its old file. check, when set, may inspect the staged
// binary and veto the swap; on any error dest is left as it was.
func (c *Client) Install(ctx context.Context, rel Release, dest string, check func(staged string) error) error {
	name := ArchiveName(rel.Tag)
	archiveURL, sumsURL := assetURL(rel, name), assetURL(rel, "checksums.txt")
	if archiveURL == "" {
		return fmt.Errorf("release %s has no %s", rel.Tag, name)
	}
	if sumsURL == "" {
		return fmt.Errorf("release %s has no checksums.txt; refusing an unverified binary", rel.Tag)
	}
	sums, err := c.download(ctx, sumsURL)
	if err != nil {
		return err
	}
	archive, err := c.download(ctx, archiveURL)
	if err != nil {
		return err
	}
	if err := Verify(archive, sums, name); err != nil {
		return err
	}
	bin, err := extract(archive)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return replace(dest, bin, check)
}

func assetURL(rel Release, name string) string {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a.URL
		}
	}
	return ""
}

// Verify checks data against its line in a sha256sum-style checksums file.
func Verify(data, sums []byte, name string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	for line := range strings.Lines(string(sums)) {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			if !strings.EqualFold(f[0], got) {
				return fmt.Errorf("checksum mismatch for %s; refusing to install it", name)
			}
			return nil
		}
	}
	return fmt.Errorf("checksums.txt lists no %s", name)
}

func extract(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no %s inside", binaryName)
		}
		if err != nil {
			return nil, err
		}
		// Either name holds the same binary, so an older release can be
		// installed too.
		if base := path.Base(h.Name); h.Typeflag == tar.TypeReg && (base == binaryName || base == oldBinaryName) {
			return io.ReadAll(io.LimitReader(tr, maxDownload))
		}
	}
}

func replace(dest string, bin []byte, check func(string) error) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+binaryName+"-*")
	if err != nil {
		return err
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }() // no-op once renamed
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	if check != nil {
		if err := check(staged); err != nil {
			return err
		}
	}
	return os.Rename(staged, dest)
}

// Compare orders release versions such as v1.2.3 and v1.2.3-rc.1, returning
// -1, 0 or 1. A version it cannot read, such as a dev build's, sorts first.
func Compare(a, b string) int {
	va, oka := parse(a)
	vb, okb := parse(b)
	if !oka || !okb {
		return cmpBool(oka, okb)
	}
	for i := range 3 {
		if c := cmpInt(va.core[i], vb.core[i]); c != 0 {
			return c
		}
	}
	// A pre-release comes before its release.
	if va.pre == "" || vb.pre == "" {
		return cmpBool(va.pre == "", vb.pre == "")
	}
	pa, pb := strings.Split(va.pre, "."), strings.Split(vb.pre, ".")
	for i := range min(len(pa), len(pb)) {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		c := strings.Compare(pa[i], pb[i])
		if ea == nil && eb == nil {
			c = cmpInt(na, nb)
		}
		if c != 0 {
			return c
		}
	}
	return cmpInt(len(pa), len(pb))
}

// MayBreak reports whether going from one release to a newer one may bring
// incompatible changes: a new major version, or before 1.0 a new minor one,
// as docs/versioning.md defines. A version it cannot read never does.
func MayBreak(from, to string) bool {
	a, oka := parse(from)
	b, okb := parse(to)
	if !oka || !okb {
		return false
	}
	return b.core[0] > a.core[0] || (b.core[0] == 0 && a.core[0] == 0 && b.core[1] > a.core[1])
}

// Major is the major version number, or -1 for a version it cannot read.
func Major(v string) int {
	if p, ok := parse(v); ok {
		return p.core[0]
	}
	return -1
}

// Between returns the stable releases newer than from and up to to, oldest
// first. Their notes are what an upgrade from from to to brings.
func Between(rs []Release, from, to string) []Release {
	var out []Release
	for _, r := range rs {
		if r.Draft || (r.Prerelease && r.Tag != to) {
			continue
		}
		if Compare(r.Tag, from) > 0 && Compare(r.Tag, to) <= 0 {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b Release) int { return Compare(a.Tag, b.Tag) })
	return out
}

type version struct {
	core [3]int
	pre  string
}

func parse(v string) (version, bool) {
	var p version
	v = strings.TrimPrefix(v, "v")
	v, _, _ = strings.Cut(v, "+")
	v, p.pre, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return p, false
	}
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return p, false
		}
		p.core[i] = n
	}
	return p, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cmpBool orders false before true.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}
