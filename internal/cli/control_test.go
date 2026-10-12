package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestPromptTextRefusesTooLong(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", maxPromptBytes-1) + "\n"))
	if got, err := promptText(cmd, nil, true); err != nil || len(got) != maxPromptBytes-1 {
		t.Fatalf("at the limit: %d bytes, %v", len(got), err)
	}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", maxPromptBytes+1)))
	if _, err := promptText(cmd, nil, true); err == nil {
		t.Fatal("over the limit: no error")
	}
}
