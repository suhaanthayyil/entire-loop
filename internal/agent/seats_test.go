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
	if !argHasFlagValue(cheap.Args, "--model", ModelCheap) {
		t.Errorf("cheap seat should default to --model %s; args=%v", ModelCheap, cheap.Args)
	}
	// Deep seat (McpBrain=true), no override → the deep default.
	deep := BuildCmd(context.Background(), SeatSpec{Role: RoleResearch, McpBrain: true}, "brief")
	if !argHasFlagValue(deep.Args, "--model", ModelDeep) {
		t.Errorf("deep seat should default to --model %s; args=%v", ModelDeep, deep.Args)
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

// TestBuildMutatingCmd_ScrubsEnv verifies the bypassPermissions build worker gets
// a minimal allowlisted environment: ambient cloud creds / API keys / tokens are
// stripped, while what claude needs to run and auth via its keychain (PATH, HOME)
// survives. It also confirms the mutating posture (bypass mode, bound cwd).
func TestBuildMutatingCmd_ScrubsEnv(t *testing.T) {
	// No t.Parallel: uses t.Setenv.
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/tester")
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-be-dropped")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-should-be-dropped")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-id-should-be-dropped")
	t.Setenv("GITHUB_TOKEN", "gh-should-be-dropped")
	t.Setenv("GH_TOKEN", "gh2-should-be-dropped")
	t.Setenv("OPENAI_API_KEY", "oai-should-be-dropped")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/path/should-be-dropped")
	t.Setenv("SOME_CUSTOM_TOKEN", "custom-token-dropped")
	t.Setenv("MY_APP_SECRET", "secret-dropped")
	t.Setenv("RANDO_VAR", "not-in-allowlist-dropped")

	spec := SeatSpec{Role: RoleBuild, BriefOnly: true, Mutating: true, RepoRoot: "/tmp/clone"}
	cmd := BuildMutatingCmd(context.Background(), spec, "brief", "/tmp/clone")

	if cmd.Env == nil {
		t.Fatalf("mutating worker must have an explicit scrubbed env, not the inherited nil env")
	}
	// Sensitive vars must be absent.
	for _, name := range []string{
		"ANTHROPIC_API_KEY", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID",
		"GITHUB_TOKEN", "GH_TOKEN", "OPENAI_API_KEY",
		"GOOGLE_APPLICATION_CREDENTIALS", "SOME_CUSTOM_TOKEN", "MY_APP_SECRET", "RANDO_VAR",
	} {
		if envHasKey(cmd.Env, name) {
			t.Errorf("scrubbed env must NOT contain %q; env=%v", name, cmd.Env)
		}
	}
	// The essentials claude needs must survive.
	if !envContains(cmd.Env, "PATH=/usr/bin:/bin") {
		t.Errorf("scrubbed env must keep PATH; env=%v", cmd.Env)
	}
	if !envContains(cmd.Env, "HOME=/home/tester") {
		t.Errorf("scrubbed env must keep HOME (claude auths from its keychain/config under HOME); env=%v", cmd.Env)
	}
	// Mutating posture: bypass mode, cwd bound to the clone dir.
	if !argHasFlagValue(cmd.Args, "--permission-mode", permissionBypass) {
		t.Errorf("mutating cmd must run --permission-mode %s; args=%v", permissionBypass, cmd.Args)
	}
	if cmd.Dir != "/tmp/clone" {
		t.Errorf("mutating cmd Dir = %q, want the clone dir /tmp/clone", cmd.Dir)
	}
}

// TestScrubbedBuildEnv_AllowlistAndDenylist unit-tests the env filter directly.
func TestScrubbedBuildEnv_AllowlistAndDenylist(t *testing.T) {
	t.Parallel()
	in := []string{
		"PATH=/bin", "HOME=/h", "USER=u", "LOGNAME=u", "LANG=en", "TERM=xterm",
		"TMPDIR=/tmp", "SHELL=/bin/zsh", "LC_ALL=C", "LC_CTYPE=UTF-8",
		"ANTHROPIC_API_KEY=x", "AWS_REGION=us", "GOOGLE_CLOUD_PROJECT=p",
		"FOO_TOKEN=t", "BAR_SECRET=s", "BAZ_KEY=k", "GH_TOKEN=g", "UNLISTED=z",
		"malformed-no-equals",
	}
	out := scrubbedBuildEnv(in)
	keep := map[string]bool{
		"PATH=/bin": true, "HOME=/h": true, "USER=u": true, "LOGNAME=u": true,
		"LANG=en": true, "TERM=xterm": true, "TMPDIR=/tmp": true, "SHELL=/bin/zsh": true,
		"LC_ALL=C": true, "LC_CTYPE=UTF-8": true,
	}
	got := map[string]bool{}
	for _, kv := range out {
		got[kv] = true
	}
	for kv := range keep {
		if !got[kv] {
			t.Errorf("scrubbedBuildEnv dropped an allowlisted var %q", kv)
		}
	}
	for _, kv := range out {
		if !keep[kv] {
			t.Errorf("scrubbedBuildEnv leaked a non-allowlisted var %q", kv)
		}
	}
}

func envHasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
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
