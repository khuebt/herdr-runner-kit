package task

import (
	"strings"
	"testing"
)

func TestParseAgentSpec(t *testing.T) {
	tests := []struct {
		in      string
		want    AgentSpec
		wantErr bool
	}{
		{in: "codex", want: AgentSpec{Harness: Codex}},
		{in: "claude:claude-opus-5-5:high", want: AgentSpec{Harness: Claude, Model: "claude-opus-5-5", Effort: "high"}},
		{in: "claude::max", want: AgentSpec{Harness: Claude, Effort: "max"}},
		{in: "codex:gpt-6-astra", want: AgentSpec{Harness: Codex, Model: "gpt-6-astra"}},
		{in: "codex::minimal", want: AgentSpec{Harness: Codex, Effort: "minimal"}},
		{in: " claude ", want: AgentSpec{Harness: Claude}},
		{in: "claude::minimal", wantErr: true},
		{in: "codex::max", wantErr: true},
		{in: "gemini", wantErr: true},
		{in: "", wantErr: true},
		{in: "claude:a:b:c", wantErr: true},
		{in: "claude:bad model", wantErr: true},
		{in: "pi:openai-codex/gpt-6-luna", want: AgentSpec{Harness: Pi, Model: "openai-codex/gpt-6-luna"}},
		{in: "pi:openai-codex/gpt-6-luna:high", want: AgentSpec{Harness: Pi, Model: "openai-codex/gpt-6-luna", Effort: "high"}},
		{in: "omp:openai-codex/gpt-6-luna:max", want: AgentSpec{Harness: Omp, Model: "openai-codex/gpt-6-luna", Effort: "max"}},
		{in: "omp:openai-codex/gpt-6-luna:auto", want: AgentSpec{Harness: Omp, Model: "openai-codex/gpt-6-luna", Effort: "auto"}},
		{in: "pi", wantErr: true},
		{in: "omp", wantErr: true},
		{in: "pi::high", wantErr: true},
		{in: "omp:", wantErr: true},
		{in: "pi:openai-codex/gpt-6-luna:auto", wantErr: true},
		{in: "omp:openai-codex/gpt 6", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseAgentSpec(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseAgentSpec(%q) = %+v, want error", tt.in, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("ParseAgentSpec(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}

func TestModelRequiredError(t *testing.T) {
	_, err := ParseAgentSpec("pi")
	want := `invalid agent "pi": pi needs a model, such as pi:openai-codex/gpt-6-luna[:effort]`
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %s", err, want)
	}
	if _, err := ParseAgentSpec("gemini"); err == nil || !strings.Contains(err.Error(), "claude, codex, pi or omp") {
		t.Errorf("unknown harness error = %v", err)
	}
}

func TestAgentSpecStringRoundTrip(t *testing.T) {
	for _, s := range []string{
		"codex", "claude:claude-opus-5-5", "claude:claude-opus-5-5:high", "claude::max",
		"pi:openai-codex/gpt-6-luna", "pi:openai-codex/gpt-6-luna:high",
		"omp:openai-codex/gpt-6-luna:max", "omp:openai-codex/gpt-6-luna:auto",
	} {
		spec, err := ParseAgentSpec(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := spec.String(); got != s {
			t.Errorf("String() = %q, want %q", got, s)
		}
	}
}

func TestParseID(t *testing.T) {
	for in, want := range map[string]string{"t3": "t3", "#t3": "t3", "T12": "t12", " #T7 ": "t7"} {
		got, _, err := ParseID(in)
		if err != nil || got != want {
			t.Errorf("ParseID(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "t", "3", "t0", "t03", "tx", "t-1", "#"} {
		if _, _, err := ParseID(in); err == nil {
			t.Errorf("ParseID(%q) succeeded, want error", in)
		}
	}
}

func TestParseAttemptRef(t *testing.T) {
	for in, want := range map[string]struct {
		id      string
		attempt int
	}{
		"t3": {"t3", 0}, "#t3a2": {"t3", 2}, "T12A1": {"t12", 1}, " #t7a10 ": {"t7", 10},
	} {
		id, attempt, err := ParseAttemptRef(in)
		if err != nil || id != want.id || attempt != want.attempt {
			t.Errorf("ParseAttemptRef(%q) = %q, %d, %v; want %q, %d", in, id, attempt, err, want.id, want.attempt)
		}
	}
	for _, in := range []string{"", "a2", "t3a", "t3a0", "t3a02", "t3ax", "t0a1", "t3a2a1", "t3b2"} {
		if _, _, err := ParseAttemptRef(in); err == nil {
			t.Errorf("ParseAttemptRef(%q) succeeded, want error", in)
		}
	}
}

func TestAttemptOf(t *testing.T) {
	for attempt, want := range map[int]int{0: 1, 1: 1, 2: 2} {
		if got := AttemptOf(&Task{Attempt: attempt}); got != want {
			t.Errorf("AttemptOf(attempt %d) = %d, want %d", attempt, got, want)
		}
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range allStatuses {
		want := s == Done || s == Failed || s == Stopped
		if s.Terminal() != want {
			t.Errorf("%s.Terminal() = %v", s, !want)
		}
		if !s.Valid() {
			t.Errorf("%s.Valid() = false", s)
		}
	}
	if Status("finished").Valid() {
		t.Error("unknown status reported valid")
	}
}

func TestWorktreeOwnsBranch(t *testing.T) {
	tests := []struct {
		name string
		w    Worktree
		want bool
	}{
		{"created", Worktree{Path: "/wt", BranchCreated: new(true)}, true},
		{"not created yet", Worktree{BranchCreated: new(false)}, false},
		{"existing branch with a path", Worktree{Path: "/wt", BranchCreated: new(false)}, false},
		{"older version with a path", Worktree{Path: "/wt"}, true},
		{"older version without a path", Worktree{}, false},
	}
	for _, tt := range tests {
		if got := tt.w.OwnsBranch(); got != tt.want {
			t.Errorf("%s: OwnsBranch = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestGitStateShort(t *testing.T) {
	tests := []struct {
		name string
		g    GitState
		want string
	}{
		{
			"uncommitted",
			GitState{Worktree: true, Branch: "hrk/t1-x", Base: "origin/main", Uncommitted: 3, Untracked: 1, AheadOfBase: 1},
			"on hrk/t1-x, 3 uncommitted, 1 untracked, 1 commit, no upstream",
		},
		{
			"not pushed",
			GitState{Worktree: true, Branch: "b", Base: "origin/main", Upstream: "origin/b", Ahead: 2, AheadOfBase: 3},
			"on b, 2 commits not pushed",
		},
		{
			"diverged",
			GitState{Worktree: true, Branch: "b", Base: "origin/main", Upstream: "origin/b", Ahead: 1, Behind: 1, AheadOfBase: 1},
			"on b, diverged from origin/b (1 ahead, 1 behind)",
		},
		{
			"behind",
			GitState{Worktree: true, Branch: "b", Base: "origin/main", Upstream: "origin/b", Behind: 2, AheadOfBase: 1},
			"on b, 2 behind origin/b",
		},
		{
			"no upstream",
			GitState{Worktree: true, Branch: "b", Base: "origin/main", AheadOfBase: 2},
			"on b, 2 commits, no upstream",
		},
		{"no new commits", GitState{Worktree: true, Branch: "b", Base: "origin/main"}, "on b, no new commits"},
		{
			"clean and pushed",
			GitState{Worktree: true, Branch: "b", Base: "origin/main", Upstream: "origin/b", AheadOfBase: 2},
			"on b, clean, pushed",
		},
		{"unknown base", GitState{Worktree: true, Branch: "b"}, "on b, clean"},
		{"repo state", GitState{Branch: "main", Base: "origin/main", Uncommitted: 3}, "repo state, on main, 3 uncommitted"},
		{"detached", GitState{Worktree: true, Branch: "(detached)", Base: "origin/main"}, "detached HEAD, no new commits"},
	}
	for _, tt := range tests {
		got := tt.g.Short()
		if got != tt.want {
			t.Errorf("%s: Short() = %q, want %q", tt.name, got, tt.want)
		}
		if strings.Contains(got, "force") {
			t.Errorf("%s: Short() suggests a force-push: %q", tt.name, got)
		}
	}
}

func TestQuestionRound(t *testing.T) {
	one := JoinQuestions([]string{"Drop it?\nyes or no"})
	if one != "Drop it?\nyes or no" || QuestionRound(one) != nil {
		t.Errorf("single question = %q, round %q", one, QuestionRound(one))
	}
	round := JoinQuestions([]string{"Which  database?\na) Postgres", "Which host?"})
	if round != "1. Which database? a) Postgres\n2. Which host?" {
		t.Errorf("round = %q", round)
	}
	if got := QuestionRound(round); len(got) != 2 || got[0] != "Which database? a) Postgres" || got[1] != "Which host?" {
		t.Errorf("QuestionRound = %q", got)
	}
	if got := QuestionRound("1. a\n3. b"); got != nil {
		t.Errorf("misnumbered round = %q", got)
	}
}

func TestShortListsStacked(t *testing.T) {
	g := &GitState{Worktree: true, Branch: "b", Base: "origin/main", AheadOfBase: 0}
	if got := g.Short(); got != "on b, no new commits" {
		t.Errorf("short = %q", got)
	}
	g.Stacked = []string{"feat/a", "feat/b"}
	if got := g.Short(); got != "on b, no new commits, stacked: feat/a, feat/b" {
		t.Errorf("short = %q", got)
	}
}
