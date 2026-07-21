package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	repo   string
	rounds int
	jobs   int
	model  string
	effort string
	goal   string
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
	cmd.Flags().IntVar(&p.rounds, "rounds", 1, "Maximum number of loop rounds")
	cmd.Flags().IntVar(&p.jobs, "jobs", 2, "Max concurrent worker seats (the CONC cap)")
	cmd.Flags().StringVar(&p.model, "model", "", "Worker model override (passed to claude --model)")
	cmd.Flags().StringVar(&p.effort, "effort", "", "Worker reasoning effort, e.g. low, medium, high")
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

	runner := agent.ExecRunner{
		RepoRoot: repoRoot,
		Dir:      repoRoot,
		Timeout:  agent.DefaultWorkerTimeout,
		RunDir:   store.Dir(),
	}
	builder := briefing.Builder{Env: briefing.Env{RepoRoot: repoRoot, DataDir: dataDir}}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "entire-loop: goal=%q repo=%s run=%s rounds=%d jobs=%d\n",
		p.goal, repoRoot, runID, p.rounds, p.jobs)

	opts := org.Options{
		Goal:     p.goal,
		Rounds:   p.rounds,
		Jobs:     p.jobs,
		Model:    p.model,
		Effort:   p.effort,
		RepoRoot: repoRoot,
		Runner:   runner,
		Briefer:  builder,
		Planner:  org.FixedPlanner{Model: p.model, Effort: p.effort, RepoRoot: repoRoot},
		Reorg:    org.NoopReorg{},
		Store:    store,
		Stdout:   out,
	}
	if _, err := org.Run(cmd.Context(), opts); err != nil {
		return err
	}
	fmt.Fprintf(out, "entire-loop: state written to %s\n", filepath.Join(store.Dir(), "state.json"))
	return nil
}

// resolveRepoRoot resolves the target repo: --repo, then ENTIRE_REPO_ROOT, then
// git top-level discovery.
func resolveRepoRoot(ctx context.Context, explicit string, env EntireEnv) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env.RepoRoot != "" {
		return env.RepoRoot, nil
	}
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
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
	report.Brain.Reachable, report.Brain.Detail = probe(ctx, "entire", "brain", "status")

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
	return nil
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
