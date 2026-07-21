// Package agent drives the worker "seats" of the loop: it builds the argv that
// runs a Claude Code worker for a seat, gates egress, spawns the process with
// full process-group reaping, and parses the JSON envelope + the seat's lenient
// result. Workers are non-mutating: they run in plan mode and PROPOSE, they
// never edit the target repo.
package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// Seat role identifiers. These match the embedded template basenames.
const (
	RoleResearch   = "research"
	RoleBuild      = "build"
	RoleCritic     = "critic"
	RoleMeasure    = "measure"
	RoleSynthesize = "synthesize"
	RoleControl    = "control"
)

// MCP configs handed to `claude --mcp-config`. The empty config (paired with
// --strict-mcp-config) turns MCP fully off for cheap seats; the brain config
// binds the entire-brain stdio server for deep seats.
const (
	emptyMCPConfig = `{"mcpServers":{}}`
	brainMCPConfig = `{"mcpServers":{"entire-brain":{"command":"entire","args":["brain","mcp"]}}}`
)

// Default worker models per seat tier, used when a seat carries no explicit
// Model (i.e. no --model override). The bare "haiku" alias does NOT resolve — it
// silently falls back to sonnet — so cheap seats pin an explicit cheap model id.
// Deep seats default to the general-purpose "sonnet". A Model override always
// wins for every seat, cheap or deep.
const (
	cheapSeatModel = "claude-haiku-4-5"
	deepSeatModel  = "sonnet"
)

// seatModel resolves the model a seat runs with: an explicit Model override wins;
// otherwise the per-tier default (deep → sonnet, cheap → an explicit cheap id).
func seatModel(spec SeatSpec) string {
	if spec.Model != "" {
		return spec.Model
	}
	if spec.McpBrain {
		return deepSeatModel
	}
	return cheapSeatModel
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
//   - Cheap seats (measure, build): BriefOnly=true, McpBrain=false — MCP is
//     turned fully OFF (--strict-mcp-config with an empty server set). They work
//     from the pre-assembled brief only.
//   - Deep seats (research, critic): McpBrain=true — the entire-brain MCP server
//     is wired in and ENTIRE_REPO_ROOT is set in the child env so brain binds the
//     repo. The MCP-off flags are NOT added for these.
//
// RepoRoot is the resolved repository root; it is exported into the child env
// for deep (McpBrain) seats so the brain server binds the right repo.
type SeatSpec struct {
	Role      string
	BriefOnly bool
	McpBrain  bool
	Model     string
	Effort    string
	RepoRoot  string
}

// SystemPrompt returns the short, stable system prompt for a seat. The heavy
// role instructions + rendered context ride on stdin (the brief); the system
// prompt only pins behavior: JSON-only output and plan-mode read-only work.
func (s SeatSpec) SystemPrompt() string {
	role := s.Role
	if role == "" {
		role = "worker"
	}
	return "You are the " + role + " seat in an Entire agent-org loop. " +
		"Follow the instructions in the message on stdin exactly. " +
		"You are in plan mode: you must NOT edit, create, or delete any file — you only read, analyze, and PROPOSE. " +
		"Respond with EXACTLY ONE JSON object and nothing else: no prose, no markdown, no code fences."
}

// BuildCmd assembles the exec.Cmd that runs a worker for spec with brief on
// stdin. It does not start the process. Egress gating is the caller's
// responsibility (see RejectForNoEgress), kept separate so BuildCmd stays a pure
// argv builder that tests can assert against.
func BuildCmd(ctx context.Context, spec SeatSpec, brief string) *exec.Cmd {
	args := []string{"--print", "--output-format", "json"}
	if model := seatModel(spec); model != "" {
		args = append(args, "--model", model)
	}
	if spec.Effort != "" {
		args = append(args, "--effort", spec.Effort)
	}
	args = append(args,
		"--permission-mode", "plan",
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

	// Every seat runs in the repo root when we have one, so relative work and the
	// brain bind the target repo rather than the orchestrator's cwd. (The runner
	// may still override Dir; see ExecRunner.Run.)
	if spec.RepoRoot != "" {
		cmd.Dir = spec.RepoRoot
	}

	// Deep seats additionally bind the brain to the repo via ENTIRE_REPO_ROOT in
	// the child env.
	if spec.McpBrain && spec.RepoRoot != "" {
		cmd.Env = append(os.Environ(), "ENTIRE_REPO_ROOT="+spec.RepoRoot)
	}
	return cmd
}
