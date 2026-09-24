package runner

import (
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// runCloneDirectories are the copies of the destination repository a run
// makes inside its own directory: the read-only tree the model stages read
// (target-base), the working copy they change (target-repo), and the fresh
// clone the validation stage installs the destination's dependencies into
// (validation-target). Nothing else a run leaves behind is within three
// orders of magnitude of their size.
//
// The per-launch agent homes are deliberately not here. Each one is
// removed when its launch ends, so what remains under agent-home is the
// empty parent, and a home that outlived its launch is the launcher's
// business rather than this one's.
var runCloneDirectories = []string{"target-base", "target-repo", "validation-target"}

// pruneRunClones removes this run's copies of the destination repository
// now that its terminal report has sealed. A run directory is kept for the
// life of the deployment, and that is right for what a finished run is
// read back for — the round history, the sealed records, the trail, the
// spend line, all of it under a megabyte. The clones are not read again by
// anything, and for a destination of any size they are gigabytes each.
// Nothing removed them: a 20 GiB volume filled after nine runs of a large
// repository, and the tenth died in its first git operation with "No space
// left on device" (live 2026-09-25).
//
// It is done here because this is the line every ending the run itself
// produces passes through in both orchestrations — the pipeline reports
// its own outcome in the runner mode, the attendant reports every stage's
// in the cards mode, and both report through this method — and because a
// run that is not finished never reaches it: a run with a question to ask
// posts the question instead of reporting, and a report the store would
// not seal returns above this call and leaves the directory whole for the
// attempt that follows.
//
// Two endings do not come from the run at all and so are not covered
// here: a question that passes its answer deadline, and a stop the
// requester asks for while the question waits. Both are sealed by the
// reception's question tick, which holds no run directory.
//
// A removal that fails is written down and changes nothing else. The
// report is already accepted; a directory that could not be cleared must
// not turn a delivered run into a failed one.
func (t *Terminal) pruneRunClones() {
	if t.workspace == "" {
		return
	}
	reclaimed := false
	for _, name := range runCloneDirectories {
		path := filepath.Join(t.workspace, name)
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
			worker.ReclaimWorkspace(t.workspace)
			err = forceRemoveAll(path)
		}
		if err != nil {
			t.logger.Error("run clone not removed", "directory", name, "reason", err.Error())
			continue
		}
		t.logger.Info("run clone removed", "directory", name)
	}
}
