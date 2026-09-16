package runner

import (
	"encoding/json"
	"errors"
	"os"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// ModelFailureDetailFile keeps what the worker knew when a reception model
// turn gave up: the phrase, the calls it made, and the last answer the
// gateway gave (finish reason, token counts, request id, status). The
// failed step file names the stage; this names the cause in numbers, so a
// reception that reasoned itself to the wall can be read from the run
// directory instead of the gateway's own logs (live 2026-09-15).
const ModelFailureDetailFile = "model-failure-detail.json"

type modelFailureDetailRecord struct {
	Step       string    `json:"step"`
	RecordedAt time.Time `json:"recorded_at"`
	worker.ModelFailureDetail
}

// recordModelFailureDetail writes the detail the failed step's stderr
// carried, or removes a stale one when it carried none. Best-effort by
// design, like the trail: the outcome never depends on it. The file is
// removed before the write so a link left at the path cannot carry the
// record outside the workspace.
func (p *Pipeline) recordModelFailureDetail(stage string) {
	path := p.path(ModelFailureDetailFile)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	detail, ok := worker.ParseFailureDetailLine(p.lastStepStderr)
	if !ok || !UsableStepName(stage) {
		return
	}
	encoded, err := json.Marshal(modelFailureDetailRecord{Step: stage, RecordedAt: time.Now().UTC(), ModelFailureDetail: detail})
	if err != nil {
		return
	}
	_ = writeRecordAtomically(path, encoded)
}

// writeRecordAtomically writes a record the status board reads from
// another process: the whole file appears at once (temp file + rename),
// never a half-written one.
func writeRecordAtomically(path string, encoded []byte) error {
	temp := path + ".tmp"
	// Removed first for the same reason the final path is: a link left
	// there must not carry the write elsewhere.
	if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
