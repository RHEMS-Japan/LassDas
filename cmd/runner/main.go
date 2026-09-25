// Command runner is the stage worker of the single-pod constitution: the
// binary a Hermes stage profile's worker.command points at. One card runs
// one stage, and the subcommand says which:
//
//   - chain-stage — one stage of a delivery's chain (implement, the
//     reviews, validate, publish, and the design stages);
//   - deliver — the delivery continuation up to a named milestone;
//   - e2e-check — the debug role's post-merge staging observation.
//
// Dispatched with the standard kanban worker environment, it works in the
// shared run directory the card names and ends with an exit code: the
// Hermes supervisor translates that into complete/block on the card, and
// the attendant reads what the stage sealed. It never touches the ledger —
// claims, questions and terminal reports belong to the attendant — never
// touches kanban.db, never talks HTTP to itself, and holds no state
// outside the run directory.
//
// Not carried over from the workflow: the operator-facing model-preflight
// operation (a smoke probe of the model endpoints). Probing is an operator
// action against the pod, not a card path.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "runner: a subcommand is required: chain-stage, deliver or e2e-check")
		os.Exit(1)
	}
	// SIGTERM from the supervisor cancels the context; every stage child
	// runs in its own process group wired to that cancel, so the whole
	// tree dies with the runner instead of orphaning a model call.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var err error
	switch os.Args[1] {
	case "chain-stage":
		err = runChainStage(ctx, os.Args[2:])
	case "deliver":
		err = runDeliver(ctx, os.Args[2:])
	case "e2e-check":
		err = runE2ECheck(ctx, os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "runner: unknown subcommand %q: chain-stage, deliver or e2e-check\n", os.Args[1])
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "runner:", err)
		os.Exit(1)
	}
}
