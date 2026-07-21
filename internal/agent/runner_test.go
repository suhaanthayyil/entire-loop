package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExecRunner_IdempotentSkip verifies that a completed seat result in the run
// dir is reused without spawning a worker. loadCached returns before BuildCmd /
// runWithReaping, so no `claude` process is started.
func TestExecRunner_IdempotentSkip(t *testing.T) {
	// No t.Parallel: clears no-egress env for a clean gate.
	t.Setenv("ENTIRE_BRAIN_NO_EGRESS", "")
	t.Setenv("ENTIRE_BRAIN_LOCAL_ONLY", "")

	dir := t.TempDir()
	// The cache is round-scoped: a round-0 research marker lives under round-0/.
	seatsDir := filepath.Join(dir, "seats", "round-0")
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
	spec := SeatSpec{Role: RoleCritic, Round: 1}
	want := SeatResult{Role: RoleCritic, OK: true, Verdict: "persisted", CostUSD: 0.7}

	r.saveCached(spec, want)

	// No temp file should survive an atomic write.
	entries, err := os.ReadDir(filepath.Join(dir, "seats", "round-1"))
	if err != nil {
		t.Fatalf("read seats dir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file after atomic write: %s", e.Name())
		}
	}

	got, ok := r.loadCached(spec)
	if !ok {
		t.Fatalf("loadCached found nothing after saveCached")
	}
	if got.Verdict != "persisted" || !got.OK || got.CostUSD != 0.7 {
		t.Errorf("round-tripped result = %+v, want persisted/OK/0.7", got)
	}
}

// TestCache_RoundScoped verifies the idempotent cache is keyed by round: a result
// saved in round 1 is NOT visible to round 2 (so round 2 cannot replay round 1),
// and the same role in a different round is a clean miss.
func TestCache_RoundScoped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := ExecRunner{RunDir: dir}
	r.saveCached(SeatSpec{Role: RoleResearch, Round: 1}, SeatResult{Role: RoleResearch, OK: true, Verdict: "r1"})

	if got, ok := r.loadCached(SeatSpec{Role: RoleResearch, Round: 1}); !ok || got.Verdict != "r1" {
		t.Fatalf("round 1 should hit its own cache; got %+v ok=%v", got, ok)
	}
	if _, ok := r.loadCached(SeatSpec{Role: RoleResearch, Round: 2}); ok {
		t.Errorf("round 2 must NOT replay round 1's cached result")
	}
}

// TestCache_OnlyOKResultsCached verifies a failed/timed-out result is never
// cached (so a transient failure cannot poison the seat) and that a stale non-OK
// marker on disk is ignored by loadCached.
func TestCache_OnlyOKResultsCached(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := ExecRunner{RunDir: dir}
	spec := SeatSpec{Role: RoleBuild, Round: 0}

	// A not-OK result is not persisted.
	r.saveCached(spec, SeatResult{Role: RoleBuild, OK: false, Warnings: []string{"boom"}})
	if _, ok := r.loadCached(spec); ok {
		t.Errorf("a not-OK result must not be cached")
	}

	// Even a non-OK marker written directly is ignored on load.
	dst := r.seatPath(spec.Round, spec.Role)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(SeatResult{Role: RoleBuild, OK: false}, "", "  ")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.loadCached(spec); ok {
		t.Errorf("loadCached must ignore a non-OK cached marker")
	}
}

// TestFinalizeResult_SurfacesTruncationAndReap verifies the runResultMeta signals
// are turned into seat warnings: a truncated stdout warns (and the result is still
// usable), and a reap failure marks the seat not-OK with a "could not be reaped"
// warning.
func TestFinalizeResult_SurfacesTruncationAndReap(t *testing.T) {
	t.Parallel()
	spec := SeatSpec{Role: RoleResearch}
	clean := `{"verdict":"ok"}`

	trunc := finalizeResult(spec, clean, "", false, nil, runResultMeta{stdoutTruncated: true}, time.Second)
	if !hasWarning(trunc.Warnings, "truncated") {
		t.Errorf("expected a truncation warning; got %v", trunc.Warnings)
	}

	reap := finalizeResult(spec, clean, "", false, nil, runResultMeta{reapFailed: true}, time.Second)
	if reap.OK {
		t.Errorf("a reap failure must mark the seat not-OK")
	}
	if !hasWarning(reap.Warnings, "could not be reaped") {
		t.Errorf("expected a reap warning; got %v", reap.Warnings)
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
