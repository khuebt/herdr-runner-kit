package task

import "testing"

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

func TestAgentSpecStringRoundTrip(t *testing.T) {
	for _, s := range []string{"codex", "claude:claude-opus-5-5", "claude:claude-opus-5-5:high", "claude::max"} {
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
