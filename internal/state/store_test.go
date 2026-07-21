package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMerge(t *testing.T) {
	t.Parallel()
	st := &State{Metrics: map[string]float64{"progress": 0.1}}

	Merge(st, RoundState{
		Round:   1,
		Metrics: map[string]float64{"progress": 0.5, "risk": 0.3},
		CostUSD: 1.25,
		GoalMet: false,
	})
	if st.Round != 1 {
		t.Errorf("round = %d, want 1", st.Round)
	}
	if len(st.Rounds) != 1 {
		t.Fatalf("rounds recorded = %d, want 1", len(st.Rounds))
	}
	if st.Metrics["progress"] != 0.5 { // last value wins
		t.Errorf("progress = %v, want 0.5", st.Metrics["progress"])
	}
	if st.Metrics["risk"] != 0.3 {
		t.Errorf("risk = %v, want 0.3", st.Metrics["risk"])
	}
	if st.TotalCostUSD != 1.25 {
		t.Errorf("total cost = %v, want 1.25", st.TotalCostUSD)
	}
	if st.GoalMet {
		t.Errorf("goalMet should still be false")
	}

	Merge(st, RoundState{Round: 2, CostUSD: 0.75, GoalMet: true})
	if st.Round != 2 {
		t.Errorf("round = %d, want 2", st.Round)
	}
	if st.TotalCostUSD != 2.0 {
		t.Errorf("total cost = %v, want 2.0", st.TotalCostUSD)
	}
	if !st.GoalMet {
		t.Errorf("goalMet should be true after a goal-met round")
	}
	if len(st.Rounds) != 2 {
		t.Errorf("rounds recorded = %d, want 2", len(st.Rounds))
	}
}

func TestMerge_NilStateIsSafe(t *testing.T) {
	t.Parallel()
	Merge(nil, RoundState{Round: 1}) // must not panic
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := NewStore(dir, "run-123")

	// Load before any Save returns (nil, nil).
	if st, err := store.Load(); err != nil || st != nil {
		t.Fatalf("Load before Save = (%v, %v), want (nil, nil)", st, err)
	}

	st := store.NewState("do the thing", time.Now())
	Merge(st, RoundState{Round: 1, Metrics: map[string]float64{"progress": 0.7}, GoalMet: true})
	if err := store.Save(st); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// state.json lands under runs/<runID>.
	wantPath := filepath.Join(dir, "runs", "run-123", "state.json")
	if store.Dir() != filepath.Join(dir, "runs", "run-123") {
		t.Errorf("Dir() = %q, want %q", store.Dir(), filepath.Dir(wantPath))
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil after Save")
	}
	if loaded.Goal != "do the thing" {
		t.Errorf("goal = %q, want %q", loaded.Goal, "do the thing")
	}
	if !loaded.GoalMet || loaded.Round != 1 {
		t.Errorf("loaded round/goalMet = %d/%v, want 1/true", loaded.Round, loaded.GoalMet)
	}
	if loaded.Metrics["progress"] != 0.7 {
		t.Errorf("loaded progress = %v, want 0.7", loaded.Metrics["progress"])
	}
	if loaded.SchemaVersion != schemaVersion {
		t.Errorf("schema version = %d, want %d", loaded.SchemaVersion, schemaVersion)
	}
}
