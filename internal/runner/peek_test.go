package runner

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestPeekRedactsTheScreen(t *testing.T) {
	r, fake, _ := newTestRunner(t)
	tk := running(t, r, SpawnOptions{Prompt: "x", Cwd: plainDir(t)})
	fake.on("pane read", ok("● Bash(export API_TOKEN=sk-live0123456789abcdefXYZ)\n❯ \n"))
	got, err := r.Peek(context.Background(), tk.ID, 30)
	if err != nil || strings.Contains(got, "sk-live0123456789") || !strings.Contains(got, "❯") {
		t.Errorf("peek = %q, %v", got, err)
	}
	if c := fake.call("pane", "read"); !slices.Equal(c[2:], []string{tk.Herdr.PaneID, "--source", "visible", "--lines", "30"}) {
		t.Errorf("pane read = %q", c)
	}
}
