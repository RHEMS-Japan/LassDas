package runner

import (
	"errors"
	"os"
	"strings"
)

// receptionCutoffMarker is the worker's own phrase for an answer the model
// ended at its output allowance (internal/worker: finish_reason=length). It
// reaches the runner only through the step's stderr; the worker exits
// non-zero without an artifact for the reception failures.
const receptionCutoffMarker = "finish_reason=length"

// noteReceptionCutoff leaves the requester a reason when a reception stage
// (the readiness pair, the contract derivation) failed because the model's
// answer was cut off at the output allowance — after the worker had already
// asked once more with the allowance widened. Without this the terminal
// comment says only that the model stage failed, and the operator finds
// the cause in the pod log (live 2026-09-05, three tickets in a row). The
// note becomes the run's trail: the reception runs no agent, so nothing
// else could have written the file, and the terminal report attaches it
// the way it attaches a delivery's trail. Best-effort: an unwritable trail
// must not change the outcome.
func (p *Pipeline) noteReceptionCutoff(stage string) {
	if !strings.Contains(p.lastStepStderr, receptionCutoffMarker) {
		return
	}
	note := "受付の AI (" + stage + ") の答えが長すぎて出力の上限で途切れたため、自動処理を止めました。" +
		"上限を広げて 1 回聞き直しましたが、それでも途切れました。同じ依頼をそのまま出し直しても同じ結果になります。" +
		"運用担当者が受付モデルの出力上限を確認します。\n"
	if err := p.writeReceptionTrail(note); err != nil {
		p.Logger.Error("reception trail not written", "error", err.Error())
	}
}

// writeReceptionTrail writes the trail this run composes for a reception
// failure. Like EnsureTrail, whatever squatted on the path is removed first
// and only the file this run wrote is trusted (trailWritten).
func (p *Pipeline) writeReceptionTrail(note string) error {
	trailPath := p.path("m1-trail.txt")
	if err := os.Remove(trailPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.WriteFile(trailPath, []byte(note), 0o600); err != nil {
		return err
	}
	p.trailWritten = true
	return nil
}
