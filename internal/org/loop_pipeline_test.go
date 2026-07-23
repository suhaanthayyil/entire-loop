package org

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// recordingBriefer captures the upstream outcomes handed to each seat so tests can
// assert that data flows across the pipeline edges.
type recordingBriefer struct {
	mu       sync.Mutex
	upstream map[string][]state.SeatOutcome
}

func (b *recordingBriefer) Brief(_ context.Context, _ string, _ *state.State, seat agent.SeatSpec, up []state.SeatOutcome) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.upstream == nil {
		b.upstream = map[string][]state.SeatOutcome{}
	}
	cp := make([]state.SeatOutcome, len(up))
	copy(cp, up)
	b.upstream[seat.Role] = cp
	return "brief:" + seat.Role, nil
}

func (b *recordingBriefer) forRole(role string) []state.SeatOutcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.upstream[role]
}

func largeDiff(marker string) string {
	var b strings.Builder
	b.WriteString("--- a/f.go\n+++ b/f.go\n")
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&b, "+added %s line %d\n", marker, i)
	}
	return b.String()
}

func outcomeWith(outcomes []state.SeatOutcome, role string) (state.SeatOutcome, bool) {
	for _, o := range outcomes {
		if o.Role == role {
			return o, true
		}
	}
	return state.SeatOutcome{}, false
}

// TestPipeline_DataFlowsAcrossEdges is the Part-B guard: the build seat's brief
// must carry the research seat's findings, and the critic seat's brief must carry
// the build seat's proposal — data crossing the edges, not a shared window.
func TestPipeline_DataFlowsAcrossEdges(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleResearch: {OK: true, Findings: []string{"RESEARCH-FINDING-X"}},
			agent.RoleBuild:    {OK: true, Proposal: "--- a/f\n+++ b/f\n+BUILD-DIFF-Y\n"},
			agent.RoleCritic:   {OK: true, GoalMet: false},
		},
	}
	br := &recordingBriefer{}
	if _, err := Run(context.Background(), Options{
		Goal: "wire data across edges", Rounds: 1, Runner: fake, Briefer: br, Store: newTestStore(t),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	buildUp := br.forRole(agent.RoleBuild)
	research, ok := outcomeWith(buildUp, agent.RoleResearch)
	if !ok {
		t.Fatalf("build's upstream must include the research seat; got %+v", buildUp)
	}
	if len(research.Findings) == 0 || research.Findings[0] != "RESEARCH-FINDING-X" {
		t.Errorf("build's upstream research finding = %v, want [RESEARCH-FINDING-X]", research.Findings)
	}

	criticUp := br.forRole(agent.RoleCritic)
	build, ok := outcomeWith(criticUp, agent.RoleBuild)
	if !ok {
		t.Fatalf("critic's upstream must include the build seat; got %+v", criticUp)
	}
	if !strings.Contains(build.Proposal, "BUILD-DIFF-Y") {
		t.Errorf("critic's upstream build proposal = %q, want it to contain BUILD-DIFF-Y", build.Proposal)
	}
}

// fakeMutatingRunner satisfies agent.MutatingBuildRunner WITHOUT spawning claude:
// it records the seat and clone handed to RunBypassInClone so a test can assert
// the loop bound the build seat to the throwaway clone (never the real repoRoot).
type fakeMutatingRunner struct {
	agent.FakeRunner
	mu           sync.Mutex
	bypassCalled bool
	bypassRoot   string
	cloneDir     string
	cloneExists  bool
}

func (f *fakeMutatingRunner) RunBypassInClone(_ context.Context, spec agent.SeatSpec, _ string, clone *agent.BuildClone) agent.SeatResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bypassCalled = true
	f.bypassRoot = spec.RepoRoot
	f.cloneDir = clone.Dir
	// The clone must exist while the worker runs.
	_, err := os.Stat(clone.Dir)
	f.cloneExists = err == nil
	return agent.SeatResult{Role: spec.Role, OK: true, Findings: []string{"built in clone"}}
}

// seatRootBriefer records the RepoRoot each seat's brief was built with.
type seatRootBriefer struct {
	mu    sync.Mutex
	roots map[string]string
}

func (b *seatRootBriefer) Brief(_ context.Context, _ string, _ *state.State, seat agent.SeatSpec, _ []state.SeatOutcome) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roots == nil {
		b.roots = map[string]string{}
	}
	b.roots[seat.Role] = seat.RepoRoot
	return "brief:" + seat.Role, nil
}

func (b *seatRootBriefer) rootFor(role string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.roots[role]
}

// initGitRepoForOrg makes a throwaway git repo with a committed file so a real
// clone can be taken. It skips when git is unavailable.
func initGitRepoForOrg(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "seed")
	return root
}

// TestMutatingSeat_BoundToCloneNotRepo is the isolation-wiring guard: with
// --allow-mutating-build on, the loop clones the target and binds BOTH the build
// seat's brief and the bypass worker to the CLONE dir — the real repoRoot is never
// handed to the mutating worker — and the clone is cleaned up after the round.
func TestMutatingSeat_BoundToCloneNotRepo(t *testing.T) {
	t.Parallel()
	repo := initGitRepoForOrg(t)

	runner := &fakeMutatingRunner{FakeRunner: agent.FakeRunner{Default: agent.SeatResult{OK: true}}}
	br := &seatRootBriefer{}
	_, err := Run(context.Background(), Options{
		Goal:     "isolate the build seat",
		Rounds:   1,
		RepoRoot: repo,
		Runner:   runner,
		Briefer:  br,
		Planner:  FixedPlanner{AllowMutating: true, RepoRoot: repo},
		Store:    newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !runner.bypassCalled {
		t.Fatal("the mutating build seat must run through RunBypassInClone")
	}
	if !runner.cloneExists {
		t.Errorf("the clone must exist while the bypass worker runs")
	}
	// The bypass worker's seat root is the CLONE, never the real repo.
	if runner.bypassRoot == repo || runner.bypassRoot == "" {
		t.Errorf("bypass worker RepoRoot = %q, must be the throwaway clone, not the real repo %q", runner.bypassRoot, repo)
	}
	if strings.HasPrefix(runner.bypassRoot, repo) {
		t.Errorf("clone %q must not live under the target repo %q", runner.bypassRoot, repo)
	}
	if !strings.HasPrefix(runner.bypassRoot, os.TempDir()) {
		t.Errorf("clone %q must live under the OS temp dir", runner.bypassRoot)
	}
	// The build seat's BRIEF was built with the clone root too (no real-path leak).
	if got := br.rootFor(agent.RoleBuild); got != runner.cloneDir {
		t.Errorf("build brief RepoRoot = %q, want the clone dir %q", got, runner.cloneDir)
	}
	if br.rootFor(agent.RoleBuild) == repo {
		t.Errorf("build brief must NOT be built against the real repoRoot")
	}
	// A non-mutating seat is still briefed against the real repo.
	if got := br.rootFor(agent.RoleResearch); got != repo {
		t.Errorf("research (non-mutating) brief RepoRoot = %q, want the real repo %q", got, repo)
	}
	// The clone is cleaned up once the round completes.
	if _, statErr := os.Stat(runner.cloneDir); !os.IsNotExist(statErr) {
		t.Errorf("clone %q must be removed after the round; stat err = %v", runner.cloneDir, statErr)
	}
}

// cloneWritingRunner is a MutatingBuildRunner that WRITES a file into the build
// clone (simulating the change the bypass worker would produce), so a test can prove
// the external measure ran INSIDE that clone rather than against the untouched repo.
type cloneWritingRunner struct {
	agent.FakeRunner
	file    string
	content string
}

func (r *cloneWritingRunner) RunBypassInClone(_ context.Context, spec agent.SeatSpec, _ string, clone *agent.BuildClone) agent.SeatResult {
	_ = os.WriteFile(filepath.Join(clone.Dir, r.file), []byte(r.content), 0o600)
	return agent.SeatResult{Role: spec.Role, OK: true, Proposal: "wrote " + r.file}
}

// TestLoop_MeasureRunsInBuildClone is the finding-2 guard: because the loop is
// propose-only (the change lives ONLY in the throwaway clone, never applied to
// repoRoot), the external MeasureEdge must run inside that clone. The build seat
// writes metric.json into the clone; the measure command reads it there. Measuring
// repoRoot (which has no metric.json) would fail and yield no external metric.
func TestLoop_MeasureRunsInBuildClone(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	repo := initGitRepoForOrg(t)
	runner := &cloneWritingRunner{
		FakeRunner: agent.FakeRunner{Default: agent.SeatResult{OK: true}},
		file:       "metric.json",
		content:    `{"progress": 0.9}`,
	}
	// Real executor (no Exec stub): `cat metric.json` runs in the measure Dir, which
	// the loop points at the clone when a mutating build ran.
	measure := &MeasureEdge{Cmd: "cat metric.json", Dir: repo}
	st, err := Run(context.Background(), Options{
		Goal:     "measure the round's actual change",
		Rounds:   1,
		RepoRoot: repo,
		Runner:   runner,
		Briefer:  staticBriefer{},
		Planner:  FixedPlanner{AllowMutating: true, RepoRoot: repo},
		Measure:  measure,
		Store:    newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := st.Rounds[0].Metrics["progress"]; got != 0.9 {
		t.Errorf("measure must run in the build clone (which has metric.json); progress = %v, want 0.9", got)
	}
}

// panicRunner panics for a chosen role and otherwise returns OK.
type panicRunner struct{ role string }

func (p panicRunner) Run(_ context.Context, spec agent.SeatSpec, _ string) agent.SeatResult {
	if spec.Role == p.role {
		panic("seat blew up")
	}
	return agent.SeatResult{Role: spec.Role, OK: true}
}

// TestRunSeat_RecoversPanic is the Part-A.2 guard: a panic in a seat becomes a
// not-OK result with a warning; the process does not crash and the round finishes.
func TestRunSeat_RecoversPanic(t *testing.T) {
	t.Parallel()
	st, err := Run(context.Background(), Options{
		Goal: "survive a panic", Rounds: 1,
		Runner: panicRunner{role: agent.RoleBuild}, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run must not surface a seat panic as an error: %v", err)
	}
	var found bool
	for _, oc := range st.Rounds[0].Seats {
		if oc.Role == agent.RoleBuild {
			found = true
			if oc.OK {
				t.Errorf("panicked build seat should be not-OK")
			}
			if !hasSubstr(oc.Warnings, "seat panicked") {
				t.Errorf("panicked seat should warn 'seat panicked'; got %v", oc.Warnings)
			}
		}
	}
	if !found {
		t.Fatalf("build seat outcome missing")
	}
}

// panicBriefer panics inside Brief for a chosen role.
type panicBriefer struct{ role string }

func (p panicBriefer) Brief(_ context.Context, _ string, _ *state.State, seat agent.SeatSpec, _ []state.SeatOutcome) (string, error) {
	if seat.Role == p.role {
		panic("brief blew up")
	}
	return "brief", nil
}

func TestRunSeat_RecoversBrieferPanic(t *testing.T) {
	t.Parallel()
	st, err := Run(context.Background(), Options{
		Goal: "survive a brief panic", Rounds: 1,
		Runner: &agent.FakeRunner{Default: agent.SeatResult{OK: true}}, Briefer: panicBriefer{role: agent.RoleResearch}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run must not surface a briefer panic: %v", err)
	}
	oc, ok := outcomeWith(seatOutcomes(st.Rounds[0]), agent.RoleResearch)
	if !ok || oc.OK || !hasSubstr(oc.Warnings, "seat panicked") {
		t.Errorf("briefer panic should degrade the seat with a panic warning; got %+v ok=%v", oc, ok)
	}
}

func seatOutcomes(rs state.RoundState) []state.SeatOutcome { return rs.Seats }

// TestVerifierOnEdge_MajoritySurvivesAccepts: a large proposal routes to the full
// audit; 2/3 verifiers survive → the proposal is accepted and goal-met stands.
func TestVerifierOnEdge_MajoritySurvivesAccepts(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleBuild:             {OK: true, Proposal: largeDiff("KEEP")},
			agent.RoleCritic:            {OK: true, GoalMet: true, Verdict: "ship"},
			agent.RoleVerifyCorrectness: {OK: true, GoalMet: true},
			agent.RoleVerifySecurity:    {OK: true, GoalMet: true},
			agent.RoleVerifyReproduce:   {OK: true, GoalMet: false},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal: "large change accepted", Rounds: 1, Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !st.GoalMet {
		t.Errorf("2/3 survivors should accept and let goal-met stand")
	}
	build, _ := outcomeWith(st.Rounds[0].Seats, agent.RoleBuild)
	if !strings.Contains(build.Proposal, "KEEP") {
		t.Errorf("accepted proposal must be retained; got %q", build.Proposal)
	}
	if st.Rounds[0].Route != "full-audit" {
		t.Errorf("large change should route full-audit; got %q", st.Rounds[0].Route)
	}
}

// TestVerifierOnEdge_MajorityRefutesDrops: 2/3 verifiers refute → the proposal is
// dropped from the result and goal-met is forced false even though the critic
// said met.
func TestVerifierOnEdge_MajorityRefutesDrops(t *testing.T) {
	t.Parallel()
	fake := &agent.FakeRunner{
		Default: agent.SeatResult{OK: true},
		Results: map[string]agent.SeatResult{
			agent.RoleBuild:             {OK: true, Proposal: largeDiff("DROP")},
			agent.RoleCritic:            {OK: true, GoalMet: true, Verdict: "ship"},
			agent.RoleVerifyCorrectness: {OK: true, GoalMet: false},
			agent.RoleVerifySecurity:    {OK: true, GoalMet: false},
			agent.RoleVerifyReproduce:   {OK: true, GoalMet: true},
		},
	}
	st, err := Run(context.Background(), Options{
		Goal: "large change refuted", Rounds: 1, Runner: fake, Briefer: staticBriefer{}, Store: newTestStore(t),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.GoalMet {
		t.Errorf("a refuted proposal must NOT let goal-met stand")
	}
	build, _ := outcomeWith(st.Rounds[0].Seats, agent.RoleBuild)
	if build.Proposal != "" {
		t.Errorf("refuted proposal must be dropped; got %q", build.Proposal)
	}
	if !hasSubstr(build.Warnings, "refuted by verifier majority") {
		t.Errorf("dropped proposal should carry the refutation warning; got %v", build.Warnings)
	}
}
