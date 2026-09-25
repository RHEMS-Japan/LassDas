package runner

import (
	"context"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// CloneDirectories are the copies of the destination repository a run
// makes inside its own directory: the read-only tree the model stages read
// (target-base), the working copy they change (target-repo), and the fresh
// clone the validation stage installs the destination's dependencies into
// (validation-target). Nothing else a run leaves behind is within three
// orders of magnitude of their size. The reception's sweep names them from
// here as well, so this list is the one place they are written down and
// nothing rewrites it.
//
// The per-launch agent homes are deliberately not here. Each one is
// removed when its launch ends, so what remains under agent-home is the
// empty parent, and a home that outlived its launch is the launcher's
// business rather than this one's.
var CloneDirectories = []string{"target-base", "target-repo", "validation-target"}

// ReclaimableCloneDirectories are the copies a run can be made to give back
// while it is still running, when the volume has filled under it and there
// is nothing else left to clear.
//
// Only the validation sandbox is here, and the other two are deliberately
// absent. The validation stage removes and re-clones its sandbox at the
// top of every attempt, so taking it costs the next attempt one clone and
// nothing else. The tree the model stages read and the working copy they
// change are made once, by the preparation, before any card exists: no
// stage rebuilds them, and a stage that found either missing would fail on
// every remaining round of the delivery. Freeing a gigabyte by ending the
// delivery is not freeing anything.
var ReclaimableCloneDirectories = []string{"validation-target"}

// CloneRefusal is one clone directory that would not go, and the reason
// it gave.
type CloneRefusal struct {
	Directory string
	Err       error
}

// CloneSweep is what one pass over a run directory did: the clones that
// are now gone, and the ones that refused.
type CloneSweep struct {
	Removed []string
	Refused []CloneRefusal
}

// RunClonesPresent reports whether any of a run's copies of the
// destination are still inside its directory. It is the cheap half of the
// work — three stats, no walk, no removal — so a caller that looks at
// every run it knows on a clock can pass over the ones already cleared
// without touching them at all.
func RunClonesPresent(workspace string) bool {
	if workspace == "" {
		return false
	}
	for _, name := range CloneDirectories {
		if _, err := os.Lstat(filepath.Join(workspace, name)); err == nil {
			return true
		}
	}
	return false
}

// PruneRunClones removes a run's copies of the destination repository now
// that the run has ended. A run directory is kept for the life of the
// deployment, and that is right for what a finished run is read back for
// — the round history, the sealed records, the trail, the spend line, all
// of it under a megabyte. The clones are not read again by anything, and
// for a destination of any size they are gigabytes each. Nothing removed
// them: a 20 GiB volume filled after nine runs of a large repository, and
// the tenth died in its first git operation with "No space left on
// device" (live 2026-09-25).
//
// It reports what it did instead of writing it down, because the two
// callers say it differently: the run's own ending names each directory
// it cleared, while the reception's sweep, which passes over every
// finished run it knows every minute, names the run and says it once.
//
// The ending waits as long as a reclaim takes. Nothing else is happening
// in that process, and a tree left lent is a tree nobody can clear later.
func PruneRunClones(workspace string) CloneSweep {
	return pruneClones(workspace, CloneDirectories, worker.ReclaimWorkspace)
}

// SweepRunClones is PruneRunClones for a caller on a clock: a reclaim it
// has to ask for is given ctx's deadline rather than the launcher's own
// pace. The request waits on the lend lock the whole runs root shares,
// which a live launch can hold for minutes; the reception passes over
// every run it knows on each tick and must not stop there for one of
// them. A reclaim cut short leaves the directory exactly as it was, and
// the next tick asks again.
func SweepRunClones(ctx context.Context, workspace string) CloneSweep {
	return pruneClones(workspace, CloneDirectories, func(root string) { worker.ReclaimWorkspaceWithin(ctx, root) })
}

// SweepReclaimableClones takes back only what a live run can lose, for the
// caller that is trying to make room for that run's next attempt rather
// than clearing up after a finished one.
func SweepReclaimableClones(ctx context.Context, workspace string) CloneSweep {
	return pruneClones(workspace, ReclaimableCloneDirectories, func(root string) { worker.ReclaimWorkspaceWithin(ctx, root) })
}

func pruneClones(workspace string, names []string, reclaim func(string)) CloneSweep {
	sweep := CloneSweep{}
	if workspace == "" {
		return sweep
	}
	reclaimed := false
	for _, name := range names {
		path := filepath.Join(workspace, name)
		if _, err := os.Lstat(path); err != nil {
			continue
		}
		// The base tree is deliberately unwritable, so a plain removal
		// refuses it from inside; forceRemoveAll opens the directories on
		// the way down.
		err := forceRemoveAll(path)
		if err != nil && !reclaimed {
			// What refused may not be this user's: a launch that died with
			// the pod can leave a tree to the agent user, and this user
			// cannot unlink another's files. Ask for it back and try once
			// more — but only here, after a removal has actually refused,
			// because the request waits on the lend lock the whole runs
			// root shares and then walks every inode of the directory,
			// which at this moment is the clones themselves. On the
			// ordinary ending nothing is lent and nothing waits.
			reclaimed = true
			reclaim(workspace)
			err = forceRemoveAll(path)
		}
		if err != nil {
			sweep.Refused = append(sweep.Refused, CloneRefusal{Directory: name, Err: err})
			continue
		}
		sweep.Removed = append(sweep.Removed, name)
	}
	return sweep
}

// pruneRunClones clears this run's clones now that its terminal report has
// sealed, and writes down what happened directory by directory.
//
// It is done here because this is the line every ending the run itself
// produces passes through — the attendant reports every stage's outcome
// through this method — and because a
// run that is not finished never reaches it: a run with a question to ask
// posts the question instead of reporting, and a report the store would
// not seal returns above this call and leaves the directory whole for the
// attempt that follows.
//
// Two endings do not come from the run at all and so never reach this
// line: a question that passes its answer deadline, and a stop the
// requester asks for while the question waits. Both are sealed by the
// reception's question tick, which holds no run directory. The reception
// sweeps those — and anything a refusal here left behind — from its own
// tick instead (attendant.SweepFinishedRunClones).
//
// A removal that fails is written down and changes nothing else. The
// report is already accepted; a directory that could not be cleared must
// not turn a delivered run into a failed one.
func (t *Terminal) pruneRunClones() {
	sweep := PruneRunClones(t.workspace)
	for _, name := range sweep.Removed {
		t.logger.Info("run clone removed", "directory", name)
	}
	for _, refusal := range sweep.Refused {
		t.logger.Error("run clone not removed", "directory", refusal.Directory, "reason", refusal.Err.Error())
	}
}
