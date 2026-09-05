package runner

import (
	"errors"
	"os"
	"strings"
)

// receptionCutoffMarker is the worker's own phrase for an answer the model
// ended at its output allowance (internal/worker: finish_reason=length). It
// reaches the runner only through the step's stderr; the worker exits
// non-zero without an artifact for the reception failures. The two phrases
// after it say whether the worker could ask again with a wider allowance
// (internal/worker converseTurn), so the note tells the requester only
// what happened.
const (
	receptionCutoffMarker  = "finish_reason=length"
	receptionCutoffAgain   = "cut off again"
	receptionCutoffCeiling = "already at the ceiling"
)

// noteReceptionCutoff leaves the requester a reason when a reception stage
// (the readiness pair, the contract derivation) failed because the model's
// answer was cut off at the output allowance. Without this the terminal
// comment says only that the model stage failed, and the operator finds
// the cause in the pod log (live 2026-09-05, three tickets in a row). The
// note becomes the run's trail: the reception runs no agent, so nothing
// else could have written the file, and the terminal report attaches it
// the way it attaches a delivery's trail. Best-effort: an unwritable trail
// must not change the outcome.
func (p *Pipeline) noteReceptionCutoff(stage string) {
	note := receptionCutoffNote(stage, p.lastStepStderr)
	if note == "" {
		return
	}
	if err := p.writeReceptionTrail(note); err != nil {
		p.Logger.Error("reception trail not written", "error", err.Error())
	}
}

// receptionCutoffNote renders the requester-facing note for a step's stderr,
// or "" when the step did not fail on a cutoff.
func receptionCutoffNote(stage, stderr string) string {
	if !strings.Contains(stderr, receptionCutoffMarker) {
		return ""
	}
	note := "受付の AI (" + stage + ") の答えが長すぎて出力の上限で途切れたため、自動処理を止めました。"
	switch {
	case strings.Contains(stderr, receptionCutoffAgain):
		note += "上限を広げて 1 回聞き直しましたが、それでも途切れました。"
	case strings.Contains(stderr, receptionCutoffCeiling):
		note += "上限は既に最大値だったため、聞き直しはできませんでした。"
	}
	note += "同じ依頼をそのまま出し直しても同じ結果になる可能性が高いです。運用担当者が受付モデルの出力上限を確認します。\n"
	return note
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
