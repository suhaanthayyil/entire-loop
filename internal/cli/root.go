// Package cli is the command surface of the entire-loop plugin. It wires the
// cobra command tree (run/status/version/doctor), resolves the repo root and
// plugin data dir, and assembles the org loop from its real implementations.
package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// knownCommands are the subcommands (and cobra internals) that must NOT be
// rewritten into a `run <goal>` invocation.
var knownCommands = map[string]bool{
	"run":              true,
	"status":           true,
	"version":          true,
	"doctor":           true,
	"help":             true,
	"completion":       true,
	"__complete":       true,
	"__completeNoDesc": true,
}

// Execute runs the entire-loop root command with the real process environment.
// It mirrors the entire-sem/entire-judge entry point: cli.Execute(version, args).
//
// A signal-cancelled context is threaded through ExecuteContext so Ctrl-C
// (SIGINT) or SIGTERM cancels cmd.Context() and, through it, the runner and the
// brief shell-outs. That cancellation is what fires the per-worker process-group
// reaping in spawn.go; without it a Ctrl-C would orphan the claude/node
// grandchildren.
func Execute(version string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := NewRootCommand(version)
	cmd.SetArgs(withDefaultCommand(args))
	return cmd.ExecuteContext(ctx)
}

// withDefaultCommand makes `run` the default: `entire loop "<goal>"` is rewritten
// to `entire loop run "<goal>"`. A leading flag or a known subcommand is left
// untouched so `--help`, `version`, etc. still work.
func withDefaultCommand(args []string) []string {
	if len(args) == 0 {
		return args
	}
	first := args[0]
	if strings.HasPrefix(first, "-") {
		return args
	}
	if knownCommands[first] {
		return args
	}
	return append([]string{"run"}, args...)
}

// NewRootCommand builds the entire-loop command tree.
func NewRootCommand(version string) *cobra.Command {
	if version == "" {
		version = "dev"
	}
	root := &cobra.Command{
		Use:           "entire-loop",
		Short:         "Self-prompting agent-org graph loop for Entire",
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `entire-loop runs a bounded self-prompting agent-org "graph loop": a goal is
planned into worker seats (research, build, critic, measure) that fan out
concurrently with hybrid per-seat brain wiring, propose non-mutating changes,
and are verified and synthesized into a round result. Rounds repeat until the
goal is met or the round budget is spent.

Workers run in plan mode and never edit the target repo — they read, analyze,
and PROPOSE.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true

	root.AddCommand(newRunCommand(version))
	root.AddCommand(newStatusCommand())
	root.AddCommand(newDoctorCommand(version))
	root.AddCommand(newVersionCommand(version))

	return root
}

func newVersionCommand(version string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{
					"provider": "loop",
					"version":  version,
				})
			}
			_, err := io.WriteString(cmd.OutOrStdout(), version+"\n")
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit the version as JSON")
	return cmd
}
