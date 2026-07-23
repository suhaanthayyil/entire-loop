package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/suhaanthayyil/entire-loop/internal/agent"
	briefing "github.com/suhaanthayyil/entire-loop/internal/context"
	"github.com/suhaanthayyil/entire-loop/internal/org"
	"github.com/suhaanthayyil/entire-loop/internal/state"
)

// probeTimeout bounds each sibling-plugin probe so doctor never hangs.
const probeTimeout = 15 * time.Second

// ---- run ----

type runParams struct {
	repo           string
	rounds         int
	maxRounds      int
	jobs           int
	model          string
	effort         string
	goal           string
	allowMutating  bool
	planner        string
	planMode       string
	measureCmd     string
	convergeMetric string
	convergeAt     float64
}

func newRunCommand(_ string) *cobra.Command {
	var p runParams
	cmd := &cobra.Command{
		Use:   "run <goal>",
		Short: "Run one bounded loop over a goal",
		Long: `Run one bounded loop: plan → fan-out worker seats → verify → synthesize.
The goal is the remaining positional arguments joined together, so both
` + "`entire loop run \"fix the flaky test\"`" + ` and ` + "`entire loop \"fix the flaky test\"`" + ` work.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.goal = strings.TrimSpace(strings.Join(args, " "))
			return runLoop(cmd, p)
		},
	}
	cmd.Flags().StringVar(&p.repo, "repo", "", "Repository root (fallback: ENTIRE_REPO_ROOT, then git discovery)")
	cmd.Flags().IntVar(&p.rounds, "rounds", 0, "Fixed number of rounds (0 = converge until dry). Alias/cap for --max-rounds")
	cmd.Flags().IntVar(&p.maxRounds, "max-rounds", org.DefaultMaxRounds, "Safety cap on rounds in converge mode")
	cmd.Flags().IntVar(&p.jobs, "jobs", 2, "Max concurrent worker seats (the CONC cap)")
	cmd.Flags().StringVar(&p.model, "model", "", "Worker model override (passed to claude --model)")
	cmd.Flags().StringVar(&p.effort, "effort", "", "Worker reasoning effort, e.g. low, medium, high")
	cmd.Flags().BoolVar(&p.allowMutating, "allow-mutating-build", false,
		"Let the build seat run a bypassPermissions coding agent in an isolated throwaway clone (default: plan-mode propose-as-text only)")
	cmd.Flags().StringVar(&p.planner, "planner", "llm",
		"Round planner: \"llm\" (self-planning control plane — an LLM control seat plans and re-plans each round; adds one control-seat cost per round) or \"fixed\" (static research/build/critic/measure roster)")
	cmd.Flags().StringVar(&p.planMode, "plan-mode", "dynamic",
		"Plan-mutability axis (orthogonal to --planner): \"dynamic\" (re-plan the seat DAG every round from state — the graph rewrites itself) or \"immutable\" (plan the whole DAG ONCE up front, disable runtime reorg, and execute it every round with bounded per-node recovery — the fixed-script position)")
	cmd.Flags().StringVar(&p.measureCmd, "measure-cmd", "",
		"External measure command (off by default). Runs each round under a timeout (own process group, killed on timeout; stdout capped; env scrubbed); its JSON stdout (e.g. {\"progress\":0.8}) is parsed into typed round metrics that OVERRIDE the claude-derived ones and feed the reorg edge. It measures the mutating-build clone when one ran, else the repo root. By itself it does NOT stop the loop — pair it with --converge-metric to make a metric threshold end the run. Must be read-only (non-mutating) — the loop never grants it write privilege")
	cmd.Flags().StringVar(&p.convergeMetric, "converge-metric", "",
		"Metric-threshold convergence (off by default). The metric name to watch, or an inline form \"name>=value\" / \"name<=value\". When set (with --converge-at, or the inline value), a round meets the goal — stopping the loop — once that metric crosses the threshold. This is the only way an external measurement stops the loop; usually paired with --measure-cmd")
	cmd.Flags().Float64Var(&p.convergeAt, "converge-at", 0,
		"Threshold value for --converge-metric (comparison is >= unless the inline \"name<=value\" form is used). Ignored when --converge-metric is unset")
	return cmd
}

func runLoop(cmd *cobra.Command, p runParams) error {
	env := EnvFromOS()

	// Fail fast under no-egress: the MVP worker is Claude Code, which can send
	// selected brain context off the local loopback.
	if err := agent.RejectForNoEgress("claude"); err != nil {
		return err
	}

	repoRoot, err := resolveRepoRoot(cmd.Context(), p.repo, env)
	if err != nil {
		return err
	}
	dataDir := env.DataDir()
	runID := newRunID()
	store := state.NewStore(dataDir, runID)

	// Reclaim any throwaway build clones a prior crashed/killed run leaked.
	agent.PruneOrphanBuildClones()

	runner := agent.ExecRunner{
		RepoRoot: repoRoot,
		Dir:      repoRoot,
		Timeout:  agent.DefaultWorkerTimeout,
		RunDir:   store.Dir(),
	}
	builder := briefing.Builder{Env: briefing.Env{RepoRoot: repoRoot, DataDir: dataDir}}

	out := cmd.OutOrStdout()
	base := org.FixedPlanner{Model: p.model, Effort: p.effort, RepoRoot: repoRoot, AllowMutating: p.allowMutating}
	planner, err := buildPlanner(p.planner, base, runner, builder, out)
	if err != nil {
		return err
	}
	planMode, err := parsePlanMode(p.planMode)
	if err != nil {
		return err
	}

	mode := "converge"
	if p.rounds > 0 {
		mode = fmt.Sprintf("fixed %d round(s)", p.rounds)
	}
	fmt.Fprintf(out, "entire-loop: goal=%q repo=%s run=%s mode=%s planner=%s plan-mode=%s max_rounds=%d jobs=%d\n",
		p.goal, repoRoot, runID, mode, plannerLabel(p.planner), planMode, p.maxRounds, p.jobs)
	if p.allowMutating {
		fmt.Fprintln(out, "entire-loop: --allow-mutating-build ON — the build seat will run a bypassPermissions coding agent in an ISOLATED throwaway clone (your repo is not touched; env is scrubbed).")
	}

	var measure *org.MeasureEdge
	if strings.TrimSpace(p.measureCmd) != "" {
		measure = &org.MeasureEdge{Cmd: p.measureCmd, Dir: repoRoot}
		fmt.Fprintf(out, "entire-loop: measure-edge ON — running %q each round for external metrics (must be read-only).\n", p.measureCmd)
	}

	convergeMetric, convergeAt, convergeBelow, err := parseConvergeSpec(p.convergeMetric, p.convergeAt)
	if err != nil {
		return err
	}
	if convergeMetric != "" {
		op := ">="
		if convergeBelow {
			op = "<="
		}
		fmt.Fprintf(out, "entire-loop: converge-metric ON — the run stops once %q %s %g.\n", convergeMetric, op, convergeAt)
	}

	opts := org.Options{
		Goal:           p.goal,
		Rounds:         p.rounds,
		MaxRounds:      p.maxRounds,
		Jobs:           p.jobs,
		Model:          p.model,
		Effort:         p.effort,
		RepoRoot:       repoRoot,
		PlanMode:       planMode,
		Measure:        measure,
		ConvergeMetric: convergeMetric,
		ConvergeAt:     convergeAt,
		ConvergeBelow:  convergeBelow,
		Runner:         runner,
		Briefer:        builder,
		Planner:        planner,
		Reorg:          org.RulesReorg{Goal: p.goal},
		Store:          store,
		Stdout:         out,
	}
	if _, err := org.Run(cmd.Context(), opts); err != nil {
		return err
	}
	fmt.Fprintf(out, "entire-loop: state written to %s\n", filepath.Join(store.Dir(), "state.json"))
	return nil
}

// buildPlanner selects the round planner from the --planner flag. "llm" (the
// default) is the self-planning control plane — an LLM control seat plans and
// re-plans each round; "fixed" is the static research/build/critic/measure roster.
// The LLM planner reuses the loop's runner and briefer to spawn its control seat
// and writes graceful-degrade notices to warn. An unknown value is an error.
func buildPlanner(mode string, base org.FixedPlanner, runner agent.Runner, briefer org.Briefer, warn io.Writer) (org.Planner, error) {
	switch mode {
	case "", "llm":
		return org.LLMPlanner{Base: base, Runner: runner, Briefer: briefer, Warn: warn}, nil
	case "fixed":
		return base, nil
	default:
		return nil, fmt.Errorf("--planner must be \"llm\" or \"fixed\", got %q", mode)
	}
}

// plannerLabel normalizes the planner mode for display.
func plannerLabel(mode string) string {
	if mode == "fixed" {
		return "fixed"
	}
	return "llm"
}

// parsePlanMode maps the --plan-mode flag onto the org plan-mutability axis.
// "dynamic" (default) re-plans every round; "immutable" freezes the DAG once and
// runs it with bounded per-node recovery. An unknown value is an error.
func parsePlanMode(mode string) (org.PlanMode, error) {
	switch mode {
	case "", "dynamic":
		return org.PlanModeDynamic, nil
	case "immutable":
		return org.PlanModeImmutable, nil
	default:
		return org.PlanModeDynamic, fmt.Errorf("--plan-mode must be \"dynamic\" or \"immutable\", got %q", mode)
	}
}

// parseConvergeSpec resolves the metric-threshold convergence config. The metric
// flag may be a bare name (paired with --converge-at, comparison >=) or an inline
// "name>=value" / "name<=value" form whose embedded value overrides --converge-at.
// An empty metric disables convergence. It returns (name, threshold, below, err),
// where below selects the <= comparison.
func parseConvergeSpec(metricFlag string, atFlag float64) (string, float64, bool, error) {
	metricFlag = strings.TrimSpace(metricFlag)
	if metricFlag == "" {
		return "", 0, false, nil
	}
	for _, op := range []string{">=", "<=", ">", "<"} {
		if idx := strings.Index(metricFlag, op); idx >= 0 {
			name := strings.TrimSpace(metricFlag[:idx])
			rest := strings.TrimSpace(metricFlag[idx+len(op):])
			if name == "" {
				return "", 0, false, fmt.Errorf("--converge-metric %q has no metric name", metricFlag)
			}
			at, err := strconv.ParseFloat(rest, 64)
			if err != nil {
				return "", 0, false, fmt.Errorf("--converge-metric %q: threshold %q is not a number", metricFlag, rest)
			}
			return name, at, strings.HasPrefix(op, "<"), nil
		}
	}
	return metricFlag, atFlag, false, nil
}

// resolveGitTimeout bounds the git top-level discovery so a wedged git can never
// block the run from starting.
const resolveGitTimeout = 5 * time.Second

// resolveRepoRoot resolves the target repo: --repo, then ENTIRE_REPO_ROOT, then
// git top-level discovery (under a short timeout).
func resolveRepoRoot(ctx context.Context, explicit string, env EntireEnv) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env.RepoRoot != "" {
		return env.RepoRoot, nil
	}
	gctx, cancel := context.WithTimeout(ctx, resolveGitTimeout)
	defer cancel()
	out, err := exec.CommandContext(gctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("resolve repo root: pass --repo or set ENTIRE_REPO_ROOT (git discovery failed: %v)", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("resolve repo root: git returned an empty top-level; pass --repo")
	}
	return root, nil
}

// newRunID builds a time-based, process-scoped run id. A clock is acceptable
// here: this is runtime state, not a reproducible workflow.
func newRunID() string {
	return fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid())
}

// ---- status ----

type runSummary struct {
	RunID     string    `json:"run_id"`
	Goal      string    `json:"goal"`
	Round     int       `json:"round"`
	Rounds    int       `json:"rounds_recorded"`
	GoalMet   bool      `json:"goal_met"`
	CostUSD   float64   `json:"total_cost_usd"`
	UpdatedAt time.Time `json:"updated_at"`
}

type statusReport struct {
	DataDir string       `json:"data_dir"`
	Runs    []runSummary `json:"runs"`
}

func newStatusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show recent loop runs and their state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runStatus(cmd *cobra.Command, asJSON bool) error {
	env := EnvFromOS()
	dataDir := env.DataDir()
	report := statusReport{DataDir: dataDir, Runs: []runSummary{}}

	runsDir := filepath.Join(dataDir, "runs")
	entries, err := os.ReadDir(runsDir)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read runs dir: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		st, loadErr := state.NewStore(dataDir, entry.Name()).Load()
		if loadErr != nil || st == nil {
			continue
		}
		report.Runs = append(report.Runs, runSummary{
			RunID:     st.RunID,
			Goal:      st.Goal,
			Round:     st.Round,
			Rounds:    len(st.Rounds),
			GoalMet:   st.GoalMet,
			CostUSD:   st.TotalCostUSD,
			UpdatedAt: st.UpdatedAt,
		})
	}
	sort.Slice(report.Runs, func(i, j int) bool {
		return report.Runs[i].RunID > report.Runs[j].RunID // newest first
	})

	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	if len(report.Runs) == 0 {
		fmt.Fprintf(out, "no runs yet (data dir: %s)\n", dataDir)
		return nil
	}
	fmt.Fprintf(out, "loop runs (data dir: %s):\n", dataDir)
	for _, r := range report.Runs {
		fmt.Fprintf(out, "  %s  round=%d/%d goal_met=%v cost=$%.4f  %q\n",
			r.RunID, r.Round, r.Rounds, r.GoalMet, r.CostUSD, truncateGoal(r.Goal))
	}
	return nil
}

func truncateGoal(goal string) string {
	goal = strings.TrimSpace(strings.ReplaceAll(goal, "\n", " "))
	if len(goal) <= 60 {
		return goal
	}
	return goal[:57] + "..."
}

// ---- doctor ----

type siblingProbe struct {
	Reachable bool   `json:"reachable"`
	Detail    string `json:"detail,omitempty"`
}

type doctorReport struct {
	Provider    string            `json:"provider"`
	Version     string            `json:"version"`
	Environment map[string]string `json:"environment"`
	DataDir     string            `json:"data_dir"`
	DataDirOK   bool              `json:"data_dir_writable"`
	DataDirErr  string            `json:"data_dir_error,omitempty"`
	NoEgress    bool              `json:"no_egress"`
	Graph       siblingProbe      `json:"graph"`
	Brain       siblingProbe      `json:"brain"`
	BrainEmpty  bool              `json:"brain_empty"`
}

func newDoctorCommand(version string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe the loop plugin environment and its siblings (graph, brain)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd, version, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit machine-readable JSON")
	return cmd
}

func runDoctor(cmd *cobra.Command, version string, asJSON bool) error {
	env := EnvFromOS()
	dataDir := env.DataDir()

	report := doctorReport{
		Provider: "loop",
		Version:  version,
		Environment: map[string]string{
			envCLIVersion:    valueOrUnset(env.CLIVersion),
			envRepoRoot:      valueOrUnset(env.RepoRoot),
			envPluginDataDir: valueOrUnset(env.PluginDataDir),
		},
		DataDir:  dataDir,
		NoEgress: agent.NoEgressMode(),
	}
	report.DataDirOK, report.DataDirErr = probeWritable(dataDir)

	// Sibling probes: never fail hard — report reachable/not.
	ctx, cancel := context.WithTimeout(cmd.Context(), probeTimeout)
	defer cancel()
	report.Graph.Reachable, report.Graph.Detail = probe(ctx, "entire", "graph", "doctor", "--json")
	report.Brain.Reachable, report.Brain.Detail = probe(ctx, "entire", "brain", "status", "--json")
	if report.Brain.Reachable {
		// The brain is reachable; check whether it actually holds data for this
		// repo. If not, doctor emits a non-fatal hint to run `entire brain refresh`.
		// Suppress the verbose status JSON from the reachable detail — it is only
		// surfaced on failure.
		report.BrainEmpty = brainHasNoData(report.Brain.Detail)
		report.Brain.Detail = ""
	}

	out := cmd.OutOrStdout()
	if asJSON {
		return json.NewEncoder(out).Encode(report)
	}
	fmt.Fprintf(out, "%s=%s\n", envCLIVersion, report.Environment[envCLIVersion])
	fmt.Fprintf(out, "%s=%s\n", envRepoRoot, report.Environment[envRepoRoot])
	fmt.Fprintf(out, "%s=%s\n", envPluginDataDir, report.Environment[envPluginDataDir])
	fmt.Fprintf(out, "data_dir=%s\n", report.DataDir)
	if report.DataDirOK {
		fmt.Fprintln(out, "data_dir_writable=true")
	} else {
		fmt.Fprintf(out, "data_dir_writable=false (%s)\n", report.DataDirErr)
	}
	fmt.Fprintf(out, "no_egress=%v\n", report.NoEgress)
	fmt.Fprintf(out, "graph=%s\n", reachableLabel(report.Graph))
	fmt.Fprintf(out, "brain=%s\n", reachableLabel(report.Brain))
	if report.BrainEmpty {
		fmt.Fprintln(out, "brain_hint=no data indexed for this repo; run 'entire brain refresh' to populate briefs (graph works statelessly)")
	}
	return nil
}

// brainHasNoData reports whether `entire brain status --json` shows an indexed
// brain with NO data sources for the repo. It parses the sources map and returns
// true only when the parse succeeds and every source is present-but-false, so an
// unparseable status, an older schema without a sources map, or a partially built
// brain never trips the hint. The hint it drives is advisory only: the loop
// degrades gracefully when the brain is empty (briefs carry a note instead).
func brainHasNoData(statusJSON string) bool {
	var s struct {
		Sources map[string]bool `json:"sources"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(statusJSON)), &s); err != nil {
		return false
	}
	if len(s.Sources) == 0 {
		return false
	}
	for _, on := range s.Sources {
		if on {
			return false
		}
	}
	return true
}

func reachableLabel(p siblingProbe) string {
	if p.Reachable {
		return "reachable"
	}
	detail := oneLineDetail(p.Detail)
	if detail == "" {
		return "not reachable"
	}
	return "not reachable (" + detail + ")"
}

func oneLineDetail(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}

// probe runs a sibling command and reports reachability plus a bounded detail.
func probe(ctx context.Context, name string, args ...string) (bool, string) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	detail := strings.TrimSpace(string(out))
	if err != nil {
		if detail == "" {
			detail = err.Error()
		}
		return false, detail
	}
	return true, detail
}

// probeWritable mirrors entire-sem's data-dir probe: MkdirAll, create a temp
// file, then remove it. It returns (false, reason) instead of erroring so doctor
// keeps reporting the rest of the environment.
func probeWritable(dir string) (bool, string) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err.Error()
	}
	probeFile, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return false, err.Error()
	}
	name := probeFile.Name()
	_ = probeFile.Close()
	if err := os.Remove(name); err != nil {
		return false, err.Error()
	}
	return true, ""
}
