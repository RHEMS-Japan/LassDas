package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
)

// The reception, run again.
//
// A delivery whose sealed readiness decision cannot be read used to end
// there: internal_failed, a comment telling the requester an internal error
// had happened, and an operator named as the next person to act. Nothing
// about that file is a decision that the request cannot be carried out. The
// reception derived it from the ticket, from the destination's checked-out
// tree and from two models that both had to agree; every one of those is
// still there, and none of them is what went wrong. What went wrong is one
// file on a volume.
//
// So the delivery runs its reception again. The unreadable record is
// removed, the round's cards are archived, and the claim goes back to the
// queue — which is the same restart the engine already performs for a
// delivery whose engine or whose destination configuration changed under it
// (chains.go). The next reception tick rebuilds the run directory from the
// ticket and seals a decision that can be read.
//
// Once. The second unreadable decision in one delivery is not a file that
// got corrupted; it is the reception sealing something unreadable, and
// asking it a third time would be the unbounded retry this engine is meant
// not to have. So the regeneration leaves a note that survives the
// rebuild — the one thing in the run directory that does, by name, in
// Pipeline.Prepare — and a delivery that already carries one ends the way
// it used to, honestly, with the contract's own named exception
// (README: 内部エラーで終わる条件).
//
// The note is also the requester's. It says, in the implementation-plan
// notice, that the reception was run twice and that what the second one
// decided is what the delivery is built on: an assumption the engine made
// on their behalf, in the one place assumptions are shown.

// receptionAgainSchemaVersion is this record's shape.
const receptionAgainSchemaVersion = 1

// receptionAgainRecord is the note one delivery leaves when its reception
// was run a second time.
type receptionAgainRecord struct {
	SchemaVersion int       `json:"schema_version"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

// receptionRunAgain reports whether this delivery's reception has already
// been run a second time.
func receptionRunAgain(runDir string) bool {
	info, err := os.Lstat(filepath.Join(runDir, runner.ReceptionAgainFile))
	return err == nil && info.Mode().IsRegular()
}

// sealReceptionAgain removes the unreadable decision and writes the note.
//
// The removal is what makes the rebuild honest rather than hopeful: the
// preparation clears the directory anyway, but a rebuild that failed before
// it got that far would otherwise leave the same unreadable record in place
// for the next tick to read and end on.
func sealReceptionAgain(runDir, reason string, now time.Time) error {
	if err := os.Remove(filepath.Join(runDir, "history", "readiness", "decision.json")); err != nil &&
		!os.IsNotExist(err) {
		return err
	}
	encoded, err := json.Marshal(receptionAgainRecord{
		SchemaVersion: receptionAgainSchemaVersion, Reason: reason, At: now.UTC(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(runDir, runner.ReceptionAgainFile), encoded, 0o600)
}

// receptionAgainAssumption is the line the implementation-plan notice
// carries when the reception was run twice. Empty when it was not.
func receptionAgainAssumption(runDir string) string {
	raw, err := os.ReadFile(filepath.Join(runDir, runner.ReceptionAgainFile))
	if err != nil || len(raw) > 4096 {
		return ""
	}
	var record receptionAgainRecord
	if json.Unmarshal(raw, &record) != nil || record.SchemaVersion != receptionAgainSchemaVersion {
		return ""
	}
	return "受付の判断を記録したファイルが読めなくなっていたため、受付をもう一度実行し、そこで出た判断を前提にしています。"
}
