package redact

import (
	"strings"
	"testing"
)

func TestRedactSecrets(t *testing.T) {
	for _, c := range []struct{ in, secret string }{
		{"the token is ghp_abcdefghijklmnopqrstuvwxyz0123456789", "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},
		{"use sk-proj-abcdefghijklmnop1234", "sk-proj-abcdefghijklmnop1234"},
		{"AWS key AKIAABCDEFGHIJKLMNOP", "AKIAABCDEFGHIJKLMNOP"},
		{"password=hunter2", "hunter2"},
		{"DB_PASSWORD: correct-horse", "correct-horse"},
		{`{"api_key": "abc123"}`, "abc123"},
		{"clone https://bob:s3cret@example.com/team/app.git", "bob:s3cret"},
		{"Authorization: Bearer abcdefgh12345678", "abcdefgh12345678"},
		{"random 9f8e7d6c5b4a39281706f5e4d3c2b1a0ffee1234", "9f8e7d6c5b4a39281706f5e4d3c2b1a0ffee1234"},
		{"bot 123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsawQ", "AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsawQ"},
	} {
		got := Secrets(c.in)
		if strings.Contains(got, c.secret) || !strings.Contains(got, Mark) {
			t.Errorf("Secrets(%q) = %q", c.in, got)
		}
	}
	for _, plain := range []string{
		"1a 2b",
		"yes, drop the legacy endpoint",
		"the key is in the team vault under deploy/prod",
		"see https://example.com/docs/setup",
		"internationalization-considerations-everywhere",
		"1Password: payments sandbox",
	} {
		if got := Secrets(plain); got != plain {
			t.Errorf("Secrets(%q) = %q", plain, got)
		}
	}
}
