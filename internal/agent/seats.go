// Package agent drives the worker "seats" of the loop: it builds the argv that
// runs a Claude Code worker for a seat, gates egress, spawns the process with
// full process-group reaping, and parses the JSON envelope + the seat's lenient
// result.
//
// Most seats are NON-mutating: they run in plan mode and PROPOSE, they never edit
// the target repo. The BUILD seat is the exception — it is MUTATING and runs in
// bypassPermissions mode (with a scrubbed env) inside an isolated throwaway git
// CLONE so it can write real code, which the runner then captures as a diff (see
// runner.go / worktree.go). A clone (not a worktree) has its own object store and
// refs, so nothing the worker does can reach the user's repo.
package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// Seat role identifiers. These match the embedded template basenames (verify-*
// roles all share the verify.md template; see templates.Load).
const (
	RoleResearch   = "research"
	RoleBuild      = "build"
	RoleCritic     = "critic"
	RoleMeasure    = "measure"
	RoleSynthesize = "synthesize"
	RoleControl    = "control"

	// Verifier-on-edge skeptic seats: three diverse lenses, each prompted to
	// REFUTE a finding/proposal before it is accepted into the round result.
	RoleVerifyCorrectness = "verify-correctness"
	RoleVerifySecurity    = "verify-security"
	RoleVerifyReproduce   = "verify-reproduce"
)

// MCP configs handed to `claude --mcp-config`. The empty config (paired with
// --strict-mcp-config) turns MCP fully off for cheap seats; the brain config
// binds the entire-brain stdio server for deep seats.
const (
	emptyMCPConfig = `{"mcpServers":{}}`
	brainMCPConfig = `{"mcpServers":{"entire-brain":{"command":"entire","args":["brain","mcp"]}}}`
)

// Permission modes handed to `claude --permission-mode`.
const (
	permissionPlan   = "plan"
	permissionBypass = "bypassPermissions"
)

// Default worker models per seat tier, used when a seat carries no explicit
// Model (i.e. no --model override). The bare "haiku" alias does NOT resolve — it
// silently falls back to sonnet — so cheap seats pin an explicit cheap model id.
// Deep seats default to the general-purpose "sonnet". A Model override always
// wins for every seat, cheap or deep.
//
// These are exported because the LLM control-plane planner allowlists exactly
// {ModelCheap, ModelDeep}: any model a plan names outside this set is replaced by
// the per-tier default, so an adversarial plan can never select an arbitrary model.
const (
	ModelCheap = "claude-haiku-4-5"
	ModelDeep  = "sonnet"
)

// seatModel resolves the model a seat runs with: an explicit Model override wins;
// otherwise the per-tier default (deep → sonnet, cheap → an explicit cheap id).
func seatModel(spec SeatSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	if spec.McpBrain {
		return ModelDeep
	}
	return ModelCheap
}

// repoBindWarning returns a non-empty warning when a deep (brain-wired) seat has
// no resolved RepoRoot: the entire-brain server would then bind the
// orchestrator's working directory instead of the target repo. Empty means the
// seat is fine. It is a pure function so the runner can surface the warning
// without silently binding the ambient cwd.
func repoBindWarning(spec SeatSpec) string {
	if spec.McpBrain && spec.RepoRoot == "" {
		return "deep seat has no repo root; brain would bind the orchestrator cwd, not the target repo"
	}
	return ""
}

// SeatSpec describes one worker seat and its hybrid brain wiring.
//
// Hybrid wiring:
//   - Cheap seats (measure, build, verifiers): BriefOnly=true, McpBrain=false —
//     MCP is turned fully OFF (--strict-mcp-config with an empty server set). They
//     work from the pre-assembled brief only.
//   - Deep seats (research, critic): McpBrain=true — the entire-brain MCP server
//     is wired in and ENTIRE_REPO_ROOT is set in the child env so brain binds the
//     repo. The MCP-off flags are NOT added for these.
//
// RepoRoot is the resolved repository root; it is exported into the child env
// for deep (McpBrain) seats so the brain server binds the right repo.
//
// Round is the loop round this seat runs in; it round-scopes the idempotent cache
// and the build worktree so round N never replays round N-1.
//
// Mutating marks the seat as one that runs in bypassPermissions mode inside an
// isolated throwaway clone (the build seat), and only when the run opted in via
// --allow-mutating-build; every other seat stays plan-mode read-only. Lens
// carries a skeptic verifier's refutation angle.
//
// Focus is extra, per-seat prompt text the LLM control plane composes for THIS
// round (how the seats "prompt each other"). It is PROMPT-ONLY: it is rendered
// into the seat's brief body via the ${FOCUS} marker and never becomes a claude
// flag, tool, MCP config, or shell input. Its length is bounded by the planner.
type SeatSpec struct {
	Role      string
	BriefOnly bool
	McpBrain  bool
	Mutating  bool
	Model     string
	Effort    string
	RepoRoot  string
	Round     int
	Lens      string
	Focus     string
}

// SystemPrompt returns the short, stable system prompt for a seat. The heavy
// role instructions + rendered context ride on stdin (the brief); the system
// prompt only pins behavior: JSON-only output and the seat's write posture.
func (s SeatSpec) SystemPrompt() string {
	role := s.Role
	if role == "" {
		role = "worker"
	}
	posture := "You are in plan mode: you must NOT edit, create, or delete any file — you only read, analyze, and PROPOSE. "
	if s.Mutating {
		posture = "You are in an isolated, throwaway git CLONE of the target repo: you MAY create, edit, and delete files to implement the change. " +
			"Nothing here touches the user's real repo — your edits are captured as a diff and the clone is discarded. Write files, but do NOT run git commit or push. "
	}
	return "You are the " + role + " seat in an Entire agent-org loop. " +
		"Follow the instructions in the message on stdin exactly. " +
		posture +
		"Respond with EXACTLY ONE JSON object and nothing else: no prose, no markdown, no code fences."
}

// BuildCmd assembles the exec.Cmd that runs a NON-mutating (plan-mode) worker for
// spec with brief on stdin. It does not start the process. Egress gating is the
// caller's responsibility (see RejectForNoEgress), kept separate so BuildCmd
// stays a pure argv builder that tests can assert against.
func BuildCmd(ctx context.Context, spec SeatSpec, brief string) *exec.Cmd {
	return buildCmd(ctx, spec, brief, permissionPlan, spec.RepoRoot)
}

// BuildMutatingCmd assembles the exec.Cmd for a MUTATING seat: it runs in
// bypassPermissions mode with its working directory bound to dir (a throwaway
// clone), so the worker can write real files. The caller owns the clone lifecycle
// (see agent.NewBuildClone / buildInClone).
//
// Critically, the bypass worker's environment is SCRUBBED down to a minimal
// allowlist: it never inherits os.Environ() wholesale, so ambient cloud creds and
// API keys (ANTHROPIC_API_KEY, AWS_*, GITHUB_TOKEN, OPENAI_API_KEY, *_TOKEN, …)
// are stripped and cannot be exfiltrated or used by an isolated bypass agent.
// claude still authenticates from its own keychain/config under HOME.
func BuildMutatingCmd(ctx context.Context, spec SeatSpec, brief, dir string) *exec.Cmd {
	cmd := buildCmd(ctx, spec, brief, permissionBypass, dir)
	cmd.Env = scrubbedBuildEnv(os.Environ())
	return cmd
}

// mutatingEnvAllowlist is the exact set of environment variables passed through to
// the bypassPermissions build worker: what claude needs to run and authenticate
// via its own keychain/config, and nothing else. LC_* locale vars pass by prefix.
var mutatingEnvAllowlist = map[string]bool{
	"PATH":    true,
	"HOME":    true,
	"USER":    true,
	"LOGNAME": true,
	"LANG":    true,
	"TERM":    true,
	"TMPDIR":  true,
	"SHELL":   true,
}

// scrubbedBuildEnv returns a minimal allowlisted environment for the bypass build
// worker from the given environ (os.Environ() in production). Only the allowlisted
// names (and LC_* locale vars) survive; every other variable is dropped. As a
// defensive second layer, any credential-shaped variable is dropped even if it
// were somehow allowlisted, so the intent is explicit at the call site.
func scrubbedBuildEnv(environ []string) []string {
	out := make([]string, 0, len(mutatingEnvAllowlist)+2)
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if isSensitiveEnvKey(k) {
			continue
		}
		if mutatingEnvAllowlist[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// isSensitiveEnvKey reports whether an env var name looks like a credential we
// must never hand a bypass worker: explicit provider keys, cloud/vendor prefixes,
// and the generic *_TOKEN / *_SECRET / *_KEY families.
func isSensitiveEnvKey(k string) bool {
	up := strings.ToUpper(k)
	switch up {
	case "ANTHROPIC_API_KEY", "GITHUB_TOKEN", "GH_TOKEN", "OPENAI_API_KEY":
		return true
	}
	if strings.HasPrefix(up, "AWS_") || strings.HasPrefix(up, "GOOGLE_") {
		return true
	}
	if strings.HasSuffix(up, "_TOKEN") || strings.HasSuffix(up, "_SECRET") || strings.HasSuffix(up, "_KEY") {
		return true
	}
	return false
}

// buildCmd is the shared argv builder for both postures. permissionMode selects
// plan vs bypassPermissions; dir is the worker's working directory.
func buildCmd(ctx context.Context, spec SeatSpec, brief, permissionMode, dir string) *exec.Cmd {
	args := []string{"--print", "--output-format", "json"}
	if model := seatModel(spec); model != "" {
		args = append(args, "--model", model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	args = append(args,
		"--permission-mode", permissionMode,
		"--setting-sources", "user",
		"--disable-slash-commands",
	)

	// Hybrid MCP wiring. Deep seats bind the brain; cheap seats turn MCP off.
	if spec.McpBrain {
		args = append(args, "--mcp-config", brainMCPConfig)
	} else {
		args = append(args, "--strict-mcp-config", "--mcp-config", emptyMCPConfig)
	}

	args = append(args, "--system-prompt", spec.SystemPrompt())

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader(brief)

	// The worker runs in dir when we have one, so relative work and the brain bind
	// the target repo (or the build worktree) rather than the orchestrator's cwd.
	if dir != "" {
		cmd.Dir = dir
	}

	// Deep seats additionally bind the brain to the repo via ENTIRE_REPO_ROOT in
	// the child env.
	if spec.McpBrain && spec.RepoRoot != "" {
		cmd.Env = append(os.Environ(), "ENTIRE_REPO_ROOT="+spec.RepoRoot)
	}
	return cmd
}
