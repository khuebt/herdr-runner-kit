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
