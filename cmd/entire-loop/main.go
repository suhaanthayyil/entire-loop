// entire-loop is an Entire CLI external command.
//
// Once built as an executable named `entire-loop`, the parent Entire CLI
// dispatches it when a user runs `entire loop`. It drives a self-prompting
// agent-org "graph loop": a bounded goal → plan → fan-out worker seats →
// verify → synthesize cycle, with brain+graph briefs and hybrid per-seat MCP
// wiring.
package main

import (
	"fmt"
	"os"

	"github.com/suhaanthayyil/entire-loop/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Execute(version, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
