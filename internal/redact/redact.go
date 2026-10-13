// Package redact hides values that look like secrets in text the kit
// records or relays.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// Mark replaces a secret value in text the kit records or relays.
const Mark = "<redacted>"

var (
	// secretURLUserinfo matches the user:password@ part of a URL.
	secretURLUserinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s@]+@`)
	// secretKeyValue matches key=value or key: value where the key names a
	// secret, such as API_KEY=..., password: ... or "token": "...".
	secretKeyValue = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|credentials?|authorization)[A-Za-z0-9_.-]*)("?\s*[:=]\s*"?)([^\s"',;]+)`)
	// secretBearer matches an HTTP authorization value.
	secretBearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// secretToken matches the shapes of issued tokens: known prefixes,
	// JWTs and Telegram bot tokens.
	secretToken = regexp.MustCompile(`\b(?:` +
		`(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}` +
		`|gh[pousr]_[A-Za-z0-9]{20,}` +
		`|github_pat_[A-Za-z0-9_]{20,}` +
		`|glpat-[A-Za-z0-9_-]{20,}` +
		`|xox[abposr]-[A-Za-z0-9-]{10,}` +
		`|(?:AKIA|ASIA)[A-Z0-9]{16}` +
		`|AIza[A-Za-z0-9_-]{35}` +
		`|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}` +
		`|\d{6,12}:[A-Za-z0-9_-]{30,}` +
		`)`)
	// secretLong matches long unbroken runs that may be a random key. A
	// slash ends a run, so file paths are never one run; looksRandom then
	// keeps slugs. A full commit id is hidden like any hex key: the two
	// cannot be told apart, and short ids stay readable.
	secretLong = regexp.MustCompile(`[A-Za-z0-9_+=-]{32,}`)
)

// Secrets replaces values that look like secrets with Mark:
// token-like strings, the value of key=value pairs whose key names a
// secret, and the userinfo of URLs. It errs towards hiding: text the kit
// relays to the main session or Telegram must never carry a secret.
func Secrets(s string) string {
	s = secretURLUserinfo.ReplaceAllString(s, "${1}"+Mark+"@")
	s = secretBearer.ReplaceAllString(s, "${1} "+Mark)
	s = secretKeyValue.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretKeyValue.FindStringSubmatch(m)
		if sub[3] == Mark || strings.EqualFold(sub[1], "1password") {
			return m
		}
		return sub[1] + sub[2] + Mark
	})
	s = secretToken.ReplaceAllString(s, Mark)
	return secretLong.ReplaceAllStringFunc(s, func(m string) string {
		if looksRandom(m) {
			return Mark
		}
		return m
	})
}

// looksRandom reports whether a long run looks like a generated key rather
// than a slug such as feat-m2-export-csv-for-admins: it mixes letters and
// digits, and either mixes cases or has no word separators.
func looksRandom(m string) bool {
	if !strings.ContainsAny(m, "0123456789") || !strings.ContainsFunc(m, unicode.IsLetter) {
		return false
	}
	mixedCase := strings.ContainsFunc(m, unicode.IsUpper) && strings.ContainsFunc(m, unicode.IsLower)
	return mixedCase || !strings.ContainsAny(m, "-_")
}

// Found reports whether s holds a value that looks like a secret.
func Found(s string) bool { return Secrets(s) != s }
