package planrun

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"

	"github.com/khuebt/herdr-runner-kit/internal/fsutil"
)

// ErrNoStatus is returned by SetStatus for a file without a status line in
// its frontmatter.
var ErrNoStatus = errors.New("no status line in the frontmatter")

var statusLineRe = regexp.MustCompile(`^status:`)

// SetStatus rewrites the status line in the leading frontmatter of file and
// keeps every other byte. The file is replaced whole, so a reader never sees
// it half written. A file without that line is left alone and gets
// ErrNoStatus.
func SetStatus(file, status string) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	start, end, ok := frontmatterBounds(data)
	if !ok {
		return fmt.Errorf("%s: %w", file, ErrNoStatus)
	}
	pos := start
	for pos < end {
		next := bytes.IndexByte(data[pos:end], '\n')
		lineEnd := end
		if next >= 0 {
			lineEnd = pos + next
		}
		line := data[pos:lineEnd]
		if statusLineRe.Match(line) {
			cr := ""
			if bytes.HasSuffix(line, []byte("\r")) {
				cr = "\r"
			}
			repl := []byte("status: " + status + cr)
			if bytes.Equal(line, repl) {
				return nil
			}
			out := make([]byte, 0, len(data)+len(repl))
			out = append(out, data[:pos]...)
			out = append(out, repl...)
			out = append(out, data[lineEnd:]...)
			info, err := os.Stat(file)
			if err != nil {
				return err
			}
			return fsutil.WriteFileAtomic(file, out, info.Mode().Perm())
		}
		pos = lineEnd + 1
	}
	return fmt.Errorf("%s: %w", file, ErrNoStatus)
}
