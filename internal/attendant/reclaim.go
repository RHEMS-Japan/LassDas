package attendant

import (
	"context"
	"errors"
	"strings"

	"automation.internal/ticket-ingress/internal/runner"
)

// The top rung: a volume with no room left is the one failure the engine
// can fix without asking anything of anyone, because what filled the volume
// is its own leavings.
//
// Nine finished deliveries filled a 20 GiB volume with copies of the
// destination nothing would read again, and the tenth died in its first git
// operation with no space left on device (live 2026-09-25). The reception
// sweeps those every minute now, but a delivery that has just met a full
// volume cannot afford to wait for the next pass: it sweeps first, here,
// and is dispatched again straight after.
//
// The order is other deliveries' leavings before this delivery's own. What
// a finished run left is worth gigabytes and will never be read; what this
// run holds is being used, and taking it costs the next attempt the time to
// make it again. There is no third hand — the tree the model stages read
// and the working copy they change are made once, before any card exists,
// and nothing rebuilds them.

// reclaimFinishedRuns takes back the copies of the destination left in the
// directories of deliveries that have ended. It is the reception's own
// sweep, asked for now rather than on its next pass.
func reclaimFinishedRuns(ctx context.Context, climb ladderClimb) error {
	if climb.services == nil || climb.services.Store == nil {
		return errors.New("the ledger is not available to name the finished runs")
	}
	return SweepFinishedRunClones(ctx, climb.config, climb.services, climb.logger)
}

// reclaimOwnSandbox takes back what this delivery itself can lose: the
// clone the verification installs the destination's dependencies into,
// which the next verification removes and makes again anyway.
//
// Nothing sealed is touched. The round histories, the ticket records and
// the proofs are what the delivery is made of and are measured in
// kilobytes; the sandbox is measured in gigabytes.
func reclaimOwnSandbox(ctx context.Context, climb ladderClimb) error {
	sweep := runner.SweepReclaimableClones(ctx, climb.runDir)
	if len(sweep.Removed) > 0 {
		climb.logger.Info("the delivery gave back its verification sandbox to make room",
			"run", climb.run.RunID, "directories", strings.Join(sweep.Removed, " "))
	}
	if len(sweep.Refused) > 0 {
		refused := make([]string, 0, len(sweep.Refused))
		for _, refusal := range sweep.Refused {
			refused = append(refused, refusal.Directory)
		}
		return errors.New("the verification sandbox would not go: " + strings.Join(refused, " ") +
			" (" + sweep.Refused[0].Err.Error() + ")")
	}
	return nil
}
