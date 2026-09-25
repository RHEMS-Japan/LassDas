package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

// Where a returned round's answer lives.
//
// A round whose agent changed nothing and explained why is answered by the
// engine and started again, so what was decided has to outlive the card
// that failed and the pod it failed in. It sits beside that round's other
// records, on the volume the run directory is on, and two readers want it:
// the instruction the same round is rendered again from, and the report,
// which says what was assumed.
//
// Unlike the round's sealed artifacts this one accumulates — a round can be
// handed back more than once — so it is rewritten rather than created
// exclusively. The attendant is its only writer, one delivery at a time.

// ReturnRecordFile is where one round's returns are kept.
func ReturnRecordFile(runDir string, round int) string {
	return filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", round), worker.ReturnRecordFileName)
}

// ReadReturns reads back what a round has been handed back with. A round
// nobody handed back reads as no record and no error.
func ReadReturns(runDir string, round int) (*worker.ReturnedRound, error) {
	if round < 1 {
		return nil, nil
	}
	return worker.ReadReturnedRoundFile(ReturnRecordFile(runDir, round))
}

// RecordReturn adds one answer to a round's record.
//
// The write is what the relaunch rests on: the round is rendered again from
// this file, so a round started again without it would hand the agent the
// instruction it has already answered. A failure here is returned rather
// than logged, and the tick tries the whole answer again.
func RecordReturn(runDir string, round int, returned worker.ReturnedWork) error {
	if round < 1 {
		return errors.New("a returned round names a round this chain does not have")
	}
	record, err := ReadReturns(runDir, round)
	if err != nil {
		return err
	}
	if record == nil {
		record = &worker.ReturnedRound{}
	}
	record.Append(round, returned)
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := ReturnRecordFile(runDir, round)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Written through a fresh file rather than over the old one: a link
	// left at the path must not carry the write somewhere else, and a
	// half-written record would read as a round of another shape.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(path, encoded, 0o600)
}
