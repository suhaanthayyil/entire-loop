package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestExecRunner_IdempotentSkip verifies that a completed seat result in the run
// dir is reused without spawning a worker. loadCached returns before BuildCmd /
// runWithReaping, so no `claude` process is started.
func TestExecRunner_IdempotentSkip(t *testing.T) {
	// No t.Parallel: clears no-egress env for a clean gate.
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")

	dir := t.TempDir()
	seatsDir := filepath.Join(dir, "seats")
	if err := os.MkdirAll(seatsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cached := SeatResult{Role: RoleResearch, OK: true, Verdict: "cached", CostUSD: 0.42}
	data, err := json.MarshalIndent(cached, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seatsDir, RoleResearch+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	r := ExecRunner{RunDir: dir}
	got := r.Run(context.Background(), SeatSpec{Role: RoleResearch}, "unused brief")

	if got.Verdict != "cached" || !got.OK || got.CostUSD != 0.42 {
		t.Fatalf("expected cached result, got %+v", got)
	}
	if !hasWarning(got.Warnings, "idempotent skip") {
		t.Errorf("expected idempotent-skip warning; got %v", got.Warnings)
	}
}

// TestSaveCached_AtomicAndReadable verifies saveCached persists a result via the
// temp+rename atomic pattern (no leftover *.tmp) and that loadCached reads it
// back. It never spawns a worker.
func TestSaveCached_AtomicAndReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := ExecRunner{RunDir: dir}
	want := SeatResult{Role: RoleCritic, OK: true, Verdict: "persisted", CostUSD: 0.7}

	r.saveCached(want)

	// No temp file should survive an atomic write.
	entries, err := os.ReadDir(filepath.Join(dir, "seats"))
	if err != nil {
		t.Fatalf("read seats dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file after atomic write: %s", e.Name())
		}
	}

	got, ok := r.loadCached(RoleCritic)
	if !ok {
		t.Fatalf("loadCached found nothing after saveCached")
	}
	if got.Verdict != "persisted" || !got.OK || got.CostUSD != 0.7 {
		t.Errorf("round-tripped result = %+v, want persisted/OK/0.7", got)
	}
}

// TestExecRunner_NoEgressReturnsWarning verifies the runner degrades (not
// panics) under no-egress: it returns a not-OK result carrying the gate error,
// and never spawns a worker.
func TestExecRunner_NoEgressReturnsWarning(t *testing.T) {
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "1")

	r := ExecRunner{}
	got := r.Run(context.Background(), SeatSpec{Role: RoleResearch}, "brief")
	if got.OK {
		t.Errorf("expected not-OK under no-egress")
	}
	if !hasWarning(got.Warnings, "no_egress") {
		t.Errorf("expected a no_egress warning; got %v", got.Warnings)
	}
}
