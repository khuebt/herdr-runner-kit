package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// gitOut runs git in dir and returns trimmed stdout, or "" on any failure.
func gitOut(ctx context.Context, dir string, args ...string) string {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Toplevel returns the git top-level of dir, or "" outside git.
func Toplevel(ctx context.Context, dir string) string {
	return gitOut(ctx, dir, "rev-parse", "--show-toplevel")
}

// RepoRoot returns the main repository root of dir, even when dir is inside
// a linked worktree, or "" outside a non-bare git repository. Codex keys its
// trust on this root, and herdr creates worktrees from it.
func RepoRoot(ctx context.Context, dir string) string {
	common := gitOut(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" || filepath.Base(common) != ".git" {
		return ""
	}
	return filepath.Dir(common)
}

// BranchExists reports whether refs/heads/branch exists in the repo at root.
func BranchExists(ctx context.Context, root, branch string) bool {
	return exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

// Slug turns text into a short lowercase branch-safe word list. Accents are
// dropped first, so "Sửa lỗi đăng nhập" becomes "sua-loi-dang-nhap".
func Slug(text string, maxLen int) string {
	var b strings.Builder
	dash := false
	for _, r := range foldAccents(strings.ToLower(text)) {
		if b.Len() >= maxLen {
			break
		}
		if r >= unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r)) {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
		dash = false
	}
	return strings.Trim(b.String()[:min(b.Len(), maxLen)], "-")
}

// BranchName returns prefix + "<id>-<slug>", such as "hrk/t3-fix-login".
func BranchName(prefix, id, title string) string {
	if s := Slug(title, 32); s != "" {
		return prefix + id + "-" + s
	}
	return prefix + id
}

// TitleFrom derives a short title from a prompt: its first line, cut to
// about 48 characters at a word boundary.
func TitleFrom(prompt string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	words := strings.Fields(line)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > 48 && b.Len() > 0 {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	return b.String()
}

// foldAccents removes combining marks after canonical decomposition, and maps
// the letters that do not decompose.
func foldAccents(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r == 'đ':
			b.WriteRune('d')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
