// Package install writes and removes the files setup owns in Claude Code's
// config folders: the main-agent skill and the auto re-arm Stop hook.
package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// A marker line records which version wrote a file and a hash of the rest of
// it. A file whose hash no longer matches was edited by the user, and setup
// leaves it alone unless forced.
const markerPrefix = "<!-- herdr-runner-kit "

func contentHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:6])
}

// Stamp returns content with a marker line for version. The marker goes
// right after YAML front matter, which must stay first for Claude to read the
// skill, or else on the first line.
func Stamp(content []byte, version string) []byte {
	marker := fmt.Sprintf("%s%s %s -->\n", markerPrefix, version, contentHash(content))
	at := 0
	if bytes.HasPrefix(content, []byte("---\n")) {
		if end := bytes.Index(content[4:], []byte("\n---\n")); end >= 0 {
			at = 4 + end + len("\n---\n")
		}
	}
	out := make([]byte, 0, len(content)+len(marker))
	out = append(out, content[:at]...)
	out = append(out, marker...)
	return append(out, content[at:]...)
}

// Marker finds the marker in data. It returns the version that wrote the
// file and whether the rest of the file is unchanged since then.
func Marker(data []byte) (version string, intact, found bool) {
	start := 0
	for start < len(data) {
		end := bytes.IndexByte(data[start:], '\n')
		if end < 0 {
			return "", false, false
		}
		line := string(data[start : start+end])
		if rest, ok := strings.CutPrefix(line, markerPrefix); ok {
			fields := strings.Fields(strings.TrimSuffix(rest, "-->"))
			if len(fields) != 2 {
				return "", false, false
			}
			body := append(append([]byte{}, data[:start]...), data[start+end+1:]...)
			return fields[0], fields[1] == contentHash(body), true
		}
		start += end + 1
	}
	return "", false, false
}
