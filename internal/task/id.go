package task

import (
	"fmt"
	"strconv"
	"strings"
)

// FormatID returns the short id for sequence number n, such as "t3". Short ids
// matter because Telegram replies are routed by typing "#t3".
func FormatID(n int) string {
	return "t" + strconv.Itoa(n)
}

// ParseID accepts "t3", "T3" or "#t3" and returns the canonical id and its
// sequence number.
func ParseID(s string) (string, int, error) {
	raw := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	digits, ok := strings.CutPrefix(raw, "t")
	if !ok || digits == "" {
		return "", 0, fmt.Errorf("invalid task id %q: want t<number>, such as t3", s)
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 || digits[0] == '0' {
		return "", 0, fmt.Errorf("invalid task id %q: want t<number>, such as t3", s)
	}
	return FormatID(n), n, nil
}

// ParseAttemptRef accepts a task id that may name an attempt, such as "t3",
// "#t3a2" or "T3A2", and returns the canonical id and the attempt, 0 when
// none is named.
func ParseAttemptRef(s string) (string, int, error) {
	raw := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	idPart, attemptPart, named := strings.Cut(raw, "a")
	id, _, err := ParseID(idPart)
	if err != nil {
		return "", 0, fmt.Errorf("invalid task id %q: want t<number>, such as t3, or t<number>a<attempt>, such as t3a2", s)
	}
	if !named {
		return id, 0, nil
	}
	n, err := strconv.Atoi(attemptPart)
	if err != nil || n < 1 || attemptPart[0] == '0' {
		return "", 0, fmt.Errorf("invalid attempt in %q: want t<number>a<attempt>, such as t3a2", s)
	}
	return id, n, nil
}
