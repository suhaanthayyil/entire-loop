package agent

import (
	"context"
	"strings"
	"testing"
)

// argHasFlagValue reports whether args contains flag immediately followed by value.
func argHasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func argHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func TestBuildCmd_DeepSeatBindsBrainMCP(t *testing.T) {
	t.Parallel()
	spec := SeatSpec{Role: RoleResearch, McpBrain: true, RepoRoot: "/tmp/repo"}
	cmd := BuildCmd(context.Background(), spec, "the brief")
	args := cmd.Args

	if args[0] != "claude" {
		t.Fatalf("argv[0] = %q, want claude", args[0])
	}
	// Deep seat: brain MCP config present, and the MCP-off flags ABSENT.
	if !argHasFlagValue(args, "--mcp-config", brainMCPConfig) {
		t.Errorf("deep seat missing brain mcp-config; args=%v", args)
	}
	if argHasFlag(args, "--strict-mcp-config") {
		t.Errorf("deep seat must NOT carry --strict-mcp-config; args=%v", args)
	}
	if argHasFlagValue(args, "--mcp-config", emptyMCPConfig) {
		t.Errorf("deep seat must NOT carry the empty mcp-config; args=%v", args)
	}
	if !argHasFlagValue(args, "--permission-mode", "plan") {
		t.Errorf("expected --permission-mode plan; args=%v", args)
	}
	// Deep seat exports ENTIRE_REPO_ROOT into the child env.
	if !envContains(cmd.Env, "ENTIRE_REPO_ROOT=/tmp/repo") {
		t.Errorf("deep seat env missing ENTIRE_REPO_ROOT=/tmp/repo; env=%v", cmd.Env)
	}
}

func TestBuildCmd_CheapSeatDisablesMCP(t *testing.T) {
	t.Parallel()
	spec := SeatSpec{Role: RoleBuild, BriefOnly: true, McpBrain: false, RepoRoot: "/tmp/repo"}
	cmd := BuildCmd(context.Background(), spec, "the brief")
	args := cmd.Args

	// Cheap seat: MCP turned fully off.
	if !argHasFlag(args, "--strict-mcp-config") {
		t.Errorf("cheap seat must carry --strict-mcp-config; args=%v", args)
	}
	if !argHasFlagValue(args, "--mcp-config", emptyMCPConfig) {
		t.Errorf("cheap seat must carry the empty mcp-config; args=%v", args)
	}
	if argHasFlagValue(args, "--mcp-config", brainMCPConfig) {
		t.Errorf("cheap seat must NOT carry the brain mcp-config; args=%v", args)
	}
	// Cheap seat does not export a repo root (MCP is off; env stays inherited).
	if cmd.Env != nil {
		t.Errorf("cheap seat should not set a custom env; got %v", cmd.Env)
	}
}

func TestBuildCmd_ModelAndEffortInjected(t *testing.T) {
	t.Parallel()
	spec := SeatSpec{Role: RoleCritic, McpBrain: true, Model: "sonnet", Effort: "low"}
	cmd := BuildCmd(context.Background(), spec, "brief")
	if !argHasFlagValue(cmd.Args, "--model", "sonnet") {
		t.Errorf("expected --model sonnet; args=%v", cmd.Args)
	}
	if !argHasFlagValue(cmd.Args, "--effort", "low") {
		t.Errorf("expected --effort low; args=%v", cmd.Args)
	}
}

func TestBuildCmd_NoEffortFlagWhenUnset(t *testing.T) {
	t.Parallel()
	spec := SeatSpec{Role: RoleMeasure, BriefOnly: true}
	cmd := BuildCmd(context.Background(), spec, "brief")
	if argHasFlag(cmd.Args, "--effort") {
		t.Errorf("no --effort expected when Effort is empty; args=%v", cmd.Args)
	}
	// The system prompt is always present and references plan mode.
	sp, ok := flagValue(cmd.Args, "--system-prompt")
	if !ok {
		t.Fatalf("expected --system-prompt; args=%v", cmd.Args)
	}
	if !strings.Contains(sp, "plan mode") || !strings.Contains(sp, "measure") {
		t.Errorf("system prompt = %q, want mention of the role and plan mode", sp)
	}
}

func TestBuildCmd_DefaultModelPerSeatTier(t *testing.T) {
	t.Parallel()
	// Cheap seat (McpBrain=false), no override → the explicit cheap model id (the
	// bare "haiku" alias does not resolve).
	cheap := BuildCmd(context.Background(), SeatSpec{Role: RoleMeasure, BriefOnly: true}, "brief")
	if !argHasFlagValue(cheap.Args, "--model", cheapSeatModel) {
		t.Errorf("cheap seat should default to --model %s; args=%v", cheapSeatModel, cheap.Args)
	}
	// Deep seat (McpBrain=true), no override → the deep default.
	deep := BuildCmd(context.Background(), SeatSpec{Role: RoleResearch, McpBrain: true}, "brief")
	if !argHasFlagValue(deep.Args, "--model", deepSeatModel) {
		t.Errorf("deep seat should default to --model %s; args=%v", deepSeatModel, deep.Args)
	}
	// An explicit override always wins, regardless of tier.
	over := BuildCmd(context.Background(), SeatSpec{Role: RoleMeasure, BriefOnly: true, Model: "opus"}, "brief")
	if !argHasFlagValue(over.Args, "--model", "opus") {
		t.Errorf("--model override should win; args=%v", over.Args)
	}
}

func TestBuildCmd_BindsRepoDir(t *testing.T) {
	t.Parallel()
	// Every seat runs in the repo root so the brain and relative work bind the
	// target repo, not the orchestrator cwd.
	deep := BuildCmd(context.Background(), SeatSpec{Role: RoleResearch, McpBrain: true, RepoRoot: "/tmp/repo"}, "brief")
	if deep.Dir != "/tmp/repo" {
		t.Errorf("deep seat Dir = %q, want /tmp/repo", deep.Dir)
	}
	cheap := BuildCmd(context.Background(), SeatSpec{Role: RoleBuild, BriefOnly: true, RepoRoot: "/tmp/repo"}, "brief")
	if cheap.Dir != "/tmp/repo" {
		t.Errorf("cheap seat Dir = %q, want /tmp/repo", cheap.Dir)
	}
}

func TestRepoBindWarning(t *testing.T) {
	t.Parallel()
	// A deep seat with no repo root degrades with a warning (never silently binds
	// the ambient cwd).
	if w := repoBindWarning(SeatSpec{Role: RoleResearch, McpBrain: true}); w == "" {
		t.Errorf("deep seat with no repo root should warn")
	}
	// Deep seat WITH a repo root, and cheap seats, do not warn.
	if w := repoBindWarning(SeatSpec{Role: RoleResearch, McpBrain: true, RepoRoot: "/tmp/repo"}); w != "" {
		t.Errorf("deep seat with a repo root should not warn; got %q", w)
	}
	if w := repoBindWarning(SeatSpec{Role: RoleBuild, McpBrain: false}); w != "" {
		t.Errorf("cheap seat should not warn; got %q", w)
	}
}

func flagValue(args []string, flag string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func envContains(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
