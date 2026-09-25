package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// ValidationFailureFile is where the validate card leaves what the
// deterministic validation refused: beside the round's other records, in the
// round that was refused rather than at the root of the run.
//
// The round matters because the reader is the round after it. An instruction
// for round N is rendered from round N-1's record, so a file at the root
// would have to be cleared by whoever writes the next one, and a clearing
// that is missed would hand a round the objection of one two rounds ago.
func ValidationFailureFile(runDir string, round int) string {
	return filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round), "validation-failure.json")
}

// SealValidationFailure writes what the deterministic validation refused, so
// the next round can be told.
//
// Best-effort by design, like the trail and the failure class: the card's
// exit code is what the kanban and the attendant act on, and a record that
// cannot be written must never change it. What is lost when it cannot be
// written is the next round's material, not the next round — that round still
// runs, on the objections alone, exactly as it did before this record existed.
func (p *Pipeline) SealValidationFailure(round int, step, output string) {
	if round < 1 || step == "" {
		return
	}
	record := worker.NewValidationFailure(round, step, output)
	record.ToolSHA = p.Config.Identity.EngineSHA
	// The same binding the round's other records carry, read from the draft
	// this run was prepared with. A record that cannot name its delivery is
	// still worth keeping: what the validation printed is the point, and the
	// reader checks the round it asked for either way.
	record.DeliveryID, _ = p.readJSONField("ticket-draft.json", "delivery_id")
	record.InputSHA256, _ = p.readJSONField("ticket-draft.json", "input_sha256")
	record.ConfigSHA256, _ = p.readJSONField("ticket-draft.json", "config_sha256")
	if record.Seal() != nil {
		return
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := ValidationFailureFile(p.Workspace, round)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Removed before the write for the reason every other record here is: a
	// link left at the path must not carry the write somewhere else. A
	// re-dispatched card that validates again overwrites, so the newest
	// account of the round wins.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = writeRecordAtomically(path, encoded)
}

// ReadValidationFailure reads back what a round's validation refused. It
// reports false for a record that is missing, unreadable, or does not bind to
// the round asked for — and a missing one is the ordinary case, because most
// rounds end on an objection and never reach the validation at all.
func ReadValidationFailure(runDir string, round int) (worker.ValidationFailure, bool) {
	if round < 1 {
		return worker.ValidationFailure{}, false
	}
	record, err := worker.ReadValidationFailureFile(ValidationFailureFile(runDir, round))
	if err != nil || !record.Bound(round) {
		return worker.ValidationFailure{}, false
	}
	return record, true
}
