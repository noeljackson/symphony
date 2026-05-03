// Command symphony is the Go reference implementation entry point.
//
// The foundation PR ships a stub binary that loads + validates a workflow
// and prints a one-line preflight result. Subsequent PRs add the dispatch
// loop, agent backends, the HTTP server, the Datastar dashboard, and the
// `symphony doctor` / `symphony logs` subcommands.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/noeljackson/symphony/go/internal/config"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: symphony [path-to-WORKFLOW.md]")
		flag.PrintDefaults()
	}
	flag.Parse()
	args := flag.Args()
	path := "./WORKFLOW.md"
	if len(args) > 0 {
		path = args[0]
	}
	def, err := config.LoadWorkflow(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "symphony: load failed: %v\n", err)
		os.Exit(2)
	}
	if err := def.Config.ValidateForDispatch(); err != nil {
		fmt.Fprintf(os.Stderr, "symphony: dispatch preflight failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("symphony: workflow %s ready (tracker=%s, backend=%s)\n",
		def.Path, def.Config.Tracker.Kind, def.Config.Agent.Backend)
}
