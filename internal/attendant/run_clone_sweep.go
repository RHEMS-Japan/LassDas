package attendant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
)

// finishedRunState is the ledger state of a run that has ended. It is the
// one state nothing leads out of: a claim that died is recovered from
// queued, claimed or report_pending, never from this one, so a run that
// reaches it will not want its working copies again.
const finishedRunState = "terminal"

// maxSealedEnvelopeBytes bounds the envelope the sweep reads to decide
// whose directory it is looking at. The same bound guards the same file
// where a runner claim reads it.
const maxSealedEnvelopeBytes = 4 * 1024 * 1024

// reclaimBudget is how long one tick will spend, in total, waiting for
// trees to be handed back by the launcher. It is the tick's and not one
// run's: a lend lock a live launch holds delays every run behind it the
// same way, and a reception that stops for each of them in turn stops for
// their sum. When it runs out the remaining runs are simply swept without
// asking, and the next tick asks again. Indirected for tests.
var reclaimBudget = 5 * time.Second

// sweepRunClones is indirected for tests.
var sweepRunClones = runner.SweepRunClones

// SweepFinishedRunClones removes the copies of the destination repository
// still sitting in the directories of runs that have ended.
//
// The run's own ending already clears them as it reports, but two endings
// do not come from the run: a question that passes its answer deadline,
// and a stop the requester asks for while the question waits. Both are
// sealed by the reception's question tick, which holds no run directory,
// so nothing cleared the clones of a run that ended either way — and
// awaiting_answer has no third exit. A removal that refused once, and
// every run that ended before anything cleared clones at all, are left
// behind the same way. So the reception passes over the runs it knows on
// its own tick: it costs three stats for a run already clear, and it is
// the only line all three of those leaks pass through.
//
// The nine finished runs that filled a 20 GiB volume left 19 GB of these
// behind, and the tenth run died in its first git operation with "No
// space left on device" (live 2026-09-25).
//
// Nothing is removed until the directory has shown itself to be that
// run's own. The path that offers one is not trusted for this: the runs
// root is configuration, a string that can name any directory on the
// volume. What is asked of the directory instead is that it hold that
// delivery's sealed envelope, which the preparation writes into a
// directory it has just cleared, before the first clone is made — so a
// directory with clones in it holds the envelope of the run that made
// them, and a directory that holds someone else's, or none, is not this
// run's and is left whole. The three names are then joined straight onto
// it, so nothing outside it is ever reached.
//
// A refusal is written down once for the run and nothing else happens:
// the next tick finds the same directory and tries again. The sweep never
// reports a failure that could end a tick — a volume that cannot be
// cleared is a reason to say so every minute, not a reason to stop
// receiving tickets — and no tick spends longer than reclaimBudget
// waiting for a tree to be handed back.
func SweepFinishedRunClones(ctx context.Context, config runtime.Config, services *runtime.Services, logger Logger) error {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	unnamed, unowned := 0, 0
	// The budget's clock starts at the first run that actually needs a
	// tree handed back, not at the top of a pass that may find nothing to
	// do at all.
	var reclaim context.Context
	var giveUp context.CancelFunc
	defer func() {
		if giveUp != nil {
			giveUp()
		}
	}()
	for _, run := range runs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if run.State != finishedRunState {
			continue
		}
		directory := finishedRunDirectory(config, run.DeliveryID)
		if directory == "" {
			unnamed++
			continue
		}
		if !runner.RunClonesPresent(directory) {
			continue
		}
		if !ownedRunDirectory(directory, run.DeliveryID) {
			unowned++
			continue
		}
		if reclaim == nil {
			reclaim, giveUp = tickReclaimBudget(ctx)
		}
		sweep := sweepRunClones(reclaim, directory)
		if len(sweep.Removed) > 0 {
			logger.Info("finished run clones removed", "run", run.RunID, "directories", strings.Join(sweep.Removed, " "))
		}
		if len(sweep.Refused) > 0 {
			refused := make([]string, 0, len(sweep.Refused))
			for _, refusal := range sweep.Refused {
				refused = append(refused, refusal.Directory)
			}
			logger.Error("finished run clones not removed", "run", run.RunID,
				"directories", strings.Join(refused, " "), "reason", sweep.Refused[0].Err.Error())
		}
	}
	if unnamed > 0 {
		logger.Info("finished runs whose directory could not be named were left alone", "runs", unnamed)
	}
	if unowned > 0 {
		logger.Error("finished runs whose directory did not hold their own envelope were left alone", "runs", unowned)
	}
	return nil
}

// tickReclaimBudget is the deadline one tick gives the launcher for
// handing trees back, held open across every run that follows it. It is
// made here, and handed back rather than used here, so that the caller
// can start its clock at the first run that actually needs a reclaim: a
// pass that finds nothing to do spends none of the budget.
func tickReclaimBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, reclaimBudget)
}

// ownedRunDirectory reports whether candidate is this delivery's own run
// directory, which is the only thing the sweep will remove anything from.
//
// It must be a directory in its own right. A run directory that is a
// symbolic link carries the three names into whatever it points at, and
// the removal follows them there.
//
// It must hold this delivery's sealed envelope. The preparation clears
// the directory and writes the envelope into it before the first clone is
// made, so every directory with clones in it has one; a directory naming
// another delivery, or naming none, is somebody else's and is left whole.
// The whole envelope is measured, not the one field that names the
// delivery: a file carrying that field and nothing else is not evidence
// of anything, and anything able to write into a directory could put one
// there. A runner claim reads the same file the same way.
func ownedRunDirectory(candidate, deliveryID string) bool {
	info, err := os.Lstat(candidate)
	if err != nil || !info.IsDir() {
		return false
	}
	sealed, err := os.Lstat(filepath.Join(candidate, "ticket-envelope.json"))
	if err != nil || !sealed.Mode().IsRegular() || sealed.Size() > maxSealedEnvelopeBytes {
		return false
	}
	envelope, err := readEnvelope(candidate, deliveryID)
	if err != nil {
		return false
	}
	return hook.ValidateEnvelope(envelope) == nil
}

// finishedRunDirectory is the delivery's directory under the configured
// root, or "" when the two do not name one directly beneath it. The sweep
// removes rather than reads, so the join is checked rather than trusted: a
// root that was never configured, or one that is relative, or one that is
// the top of a volume with everything on it beneath, or a delivery id
// carrying a separator, must not resolve to somewhere else on the volume.
func finishedRunDirectory(config runtime.Config, deliveryID string) string {
	if config.Chain.RunsRoot == "" || deliveryID == "" {
		return ""
	}
	root := filepath.Clean(config.Chain.RunsRoot)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return ""
	}
	directory := filepath.Clean(runDirectory(config, deliveryID))
	if filepath.Dir(directory) != root {
		return ""
	}
	return directory
}
