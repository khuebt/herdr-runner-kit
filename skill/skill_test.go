package skill

import (
	"bytes"
	"testing"
)

// The main agent loads SKILL.md on every session, so it must stay cheap.
func TestSkillStaysShort(t *testing.T) {
	data, err := Files.ReadFile("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(data, []byte("\n")); n > 150 {
		t.Fatalf("SKILL.md has %d lines; keep it at 150 or fewer and move detail to references/", n)
	}
}
