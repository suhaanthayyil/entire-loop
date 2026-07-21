package briefing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

func TestBuildBrief_SubstitutesAndDegrades(t *testing.T) {
	t.Parallel()
	// Graph is down; `brain query` answers. The brief must carry the goal, the
	// graceful graph note, and the brain content — never an error.
	fakeExec := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "graph" {
			return nil, errors.New("graph down")
		}
		if len(args) >= 2 && args[0] == "brain" && args[1] == "query" {
			return []byte("DURABLE-BRAIN-CONTEXT"), nil
		}
		return nil, errors.New("unexpected command")
	}
	b := Builder{
		Env:  Env{RepoRoot: "/repo"},
		Exec: fakeExec,
	}
	brief, err := b.Brief(context.Background(), "make ingest deterministic", nil, agent.SeatSpec{Role: agent.RoleResearch})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	if !strings.Contains(brief, "make ingest deterministic") {
		t.Errorf("brief missing goal; got:\n%s", brief)
	}
	if !strings.Contains(brief, "graph unavailable") {
		t.Errorf("brief missing graceful graph note; got:\n%s", brief)
	}
	if !strings.Contains(brief, "DURABLE-BRAIN-CONTEXT") {
		t.Errorf("brief missing brain context; got:\n%s", brief)
	}
	// No unfilled markers should survive rendering.
	for _, marker := range []string{"${GOAL}", "${GRAPH}", "${BRIEF}", "${STATE}", "${METRICS}"} {
		if strings.Contains(brief, marker) {
			t.Errorf("brief still contains unfilled marker %q", marker)
		}
	}
}

func TestBuildBrief_BrainQueryIsPrimary(t *testing.T) {
	t.Parallel()
	// `brain query` is the one and only brain call now (`brain context` is not a
	// real subcommand). The brief must NOT probe `brain context` at all.
	fakeExec := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 1 && args[0] == "graph":
			return []byte(`{"symbol":"Foo"}`), nil
		case len(args) >= 2 && args[0] == "brain" && args[1] == "context":
			return nil, errors.New("unknown command \"context\"")
		case len(args) >= 2 && args[0] == "brain" && args[1] == "query":
			return []byte("BRAIN-QUERY-RESULT"), nil
		}
		return nil, errors.New("unexpected")
	}
	b := Builder{Exec: fakeExec}
	brief, err := b.Brief(context.Background(), "goal", nil, agent.SeatSpec{Role: agent.RoleCritic})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	if !strings.Contains(brief, "BRAIN-QUERY-RESULT") {
		t.Errorf("brief should carry the brain query result; got:\n%s", brief)
	}
}

func TestBuildBrief_BrainQueryHangIsGracefulNote(t *testing.T) {
	t.Parallel()
	// A brain query that HANGS is bounded by the per-call timeout ctx: the ctx is
	// cancelled and the call returns an error, which must fall through to the
	// graceful note rather than stalling the loop.
	fakeExec := func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "graph" {
			return []byte(`{"symbol":"Foo"}`), nil
		}
		// Simulate a hang: block until the per-call ctx deadline/cancel fires.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b := Builder{Exec: fakeExec}
	brief, err := b.Brief(newShortCtx(t), "goal", nil, agent.SeatSpec{Role: agent.RoleCritic})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	if !strings.Contains(brief, "brain context unavailable") {
		t.Errorf("a hung brain query must degrade to a graceful note; got:\n%s", brief)
	}
	if !strings.Contains(brief, "Foo") {
		t.Errorf("graph content should still be present; got:\n%s", brief)
	}
}

// newShortCtx returns a context that cancels quickly so a simulated sibling hang
// is observed without waiting out the full siblingTimeout.
func newShortCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)
	return ctx
}

func TestBoundString_RuneBoundary(t *testing.T) {
	t.Parallel()
	// "é" is 2 bytes (0xC3 0xA9). A cut that lands mid-rune must back up to a rune
	// boundary so the result is always valid UTF-8.
	s := strings.Repeat("é", 10) // 20 bytes
	for max := 1; max <= len(s); max++ {
		got := boundString(s, max)
		if len(got) > max {
			t.Fatalf("boundString(%q, %d) len = %d, want <= %d", s, max, len(got), max)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("boundString(%q, %d) = %q is not valid UTF-8", s, max, got)
		}
	}
}

func TestBuildBrief_BoundedToMaxBytes(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 500*1024)
	fakeExec := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if len(args) >= 1 && args[0] == "graph" {
			return []byte(big), nil
		}
		return []byte(big), nil
	}
	b := Builder{Exec: fakeExec, MaxBytes: 4096}
	brief, err := b.Brief(context.Background(), "goal", nil, agent.SeatSpec{Role: agent.RoleBuild})
	if err != nil {
		t.Fatalf("Brief: %v", err)
	}
	if len(brief) > 4096 {
		t.Errorf("brief length = %d, want <= 4096", len(brief))
	}
}

func TestCompactState_Summarizes(t *testing.T) {
	t.Parallel()
	s := &state.State{
		Round:   2,
		GoalMet: false,
		Rounds: []state.RoundState{
			{Round: 1},
			{Round: 2, Verdict: "close but not done", Seats: []state.SeatOutcome{
				{Role: "research", Findings: []string{"found the parser"}},
				{Role: "build", Proposal: "--- a/x\n+++ b/x\n"},
			}},
		},
	}
	out := compactState(s)
	if !strings.Contains(out, "close but not done") {
		t.Errorf("compactState missing verdict; got:\n%s", out)
	}
	if !strings.Contains(out, "research") || !strings.Contains(out, "found the parser") {
		t.Errorf("compactState missing seat findings; got:\n%s", out)
	}
	if !strings.Contains(out, "proposed diff") {
		t.Errorf("compactState should note the proposed diff; got:\n%s", out)
	}
}

func TestCompactState_NoPriorRounds(t *testing.T) {
	t.Parallel()
	if got := compactState(nil); got != "(no prior rounds)" {
		t.Errorf("compactState(nil) = %q", got)
	}
	if got := compactState(&state.State{}); got != "(no prior rounds)" {
		t.Errorf("compactState(empty) = %q", got)
	}
}
