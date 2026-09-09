package runner

import (
	"errors"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/worker"
)

// receptionCutoffMarker is the worker's own phrase for an answer the model
// ended at its output allowance (internal/worker: finish_reason=length). It
// reaches the runner only through the step's stderr; the worker exits
// non-zero without an artifact for the reception failures. The two phrases
// after it say whether the worker could ask again with a wider allowance
// (internal/worker converseTurn), so the note tells the requester only
// what happened.
const receptionCutoffMarker = "finish_reason=" + worker.ChatFinishLength

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
	note := receptionNote(stage, p.lastStepStderr)
	if note == "" {
		return
	}
	if err := p.writeReceptionTrail(note); err != nil {
		p.Logger.Error("reception trail not written", "error", err.Error())
	}
}

// noFileChosenMarker is the worker's own phrase for a derivation whose
// model answered that none of the offered paths can carry the change
// (internal/worker DeriveTargetFiles). Like the cutoff marker it reaches
// the runner only through the step's stderr.
const noFileChosenReason = "contract derivation failed: " + worker.NoTargetFileChosen

// noFileChosen reports whether the derivation itself ended for want of a
// file to change. The reason must sit where the worker writes its own
// reason — right after the step's prefix — because the same line carries
// the head of the model's answer, and the requester's words reach that
// answer: a phrase anywhere in the line would let a ticket choose the note
// the requester is shown.
func noFileChosen(stderr string) bool {
	for _, line := range strings.Split(stderr, "\n") {
		reason := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "worker:"))
		if strings.HasPrefix(reason, noFileChosenReason) {
			return true
		}
	}
	return false
}

// deriveStage is the reception stage whose failure the no-file note explains.
// The note names what to do about a derivation, so it must not appear under
// the readiness stages even if their models write the same words.
const deriveStage = "契約の導出"

// workerLinePrefix begins every line the worker writes about its own failure.
const workerLinePrefix = "worker: "

// receptionErrorText is what the worker itself wrote as the cause on one of
// its own failure lines, or "" for any other line. The worker writes
// `worker: <what failed>: <cause>`, so the cause begins right after the
// second separator — the one position on the line the requester's words can
// never reach, because the head of a model answer is carried further along.
// Reading the cause anywhere on the line would let a ticket choose the note
// its own requester is shown.
func receptionErrorText(line string) string {
	rest := strings.TrimSpace(line)
	if !strings.HasPrefix(rest, workerLinePrefix) {
		return ""
	}
	_, cause, found := strings.Cut(strings.TrimPrefix(rest, workerLinePrefix), ": ")
	if !found {
		return ""
	}
	return cause
}

// receptionCauseOf returns the worker's own cause on the last line whose
// cause begins with the given phrase, or "" when no line does. Everything
// the notes read comes through here, so a phrase can only be matched where
// the worker wrote it.
func receptionCauseOf(stderr, phrase string) string {
	found := ""
	for _, line := range strings.Split(stderr, "\n") {
		if cause := receptionErrorText(line); strings.HasPrefix(cause, phrase) {
			found = cause
		}
	}
	return found
}

// receptionCauseNote renders the requester-facing note for the reception
// failures that are not about the shape of one answer: the model could not
// be reached, or it never answered in the shape the contract asks for. Both
// are told as what happened and what the requester can do, because those are
// the two things the note is for.
func receptionCauseNote(stage, stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		cause := receptionErrorText(line)
		switch {
		// Asked again and still nothing: the spent allowance, the
		// provider's own error, and the one gateway status that comes
		// after its retries were spent. Only these three may tell a
		// requester that sending the same ticket again is worth doing.
		case strings.HasPrefix(cause, worker.TransportFailedPhrase+": "+worker.SpentAllowancePhrase),
			strings.HasPrefix(cause, worker.ProviderEndedTurnPhrase),
			strings.HasPrefix(cause, worker.TransportFailedPhrase) && strings.Contains(cause, worker.AttemptsExhaustedPhrase):
			return "受付の AI (" + stage + ") に問い合わせましたが、応答を得られませんでした。" +
				"規定の回数まで聞き直した上での結果です。一時的な混雑で起きることが多いため、" +
				"同じ依頼をそのまま出し直すと通る場合があります。\n"
		// A limit that waiting does not lift. An exhausted balance is one
		// of these, and telling its requester to send the ticket again
		// would send them round the same wall with nobody looking at the
		// balance.
		case strings.HasPrefix(cause, worker.TransportFailedPhrase) &&
			(strings.Contains(cause, worker.LimitNotLiftedPhrase) || strings.Contains(cause, worker.RetryAfterTooLongPhrase)):
			return "受付の AI (" + stage + ") への問い合わせが、利用の上限に当たって断られました。" +
				"時間をおいて出し直しても同じ結果になります。運用担当者が利用枠を確認します。\n"
		// Everything else the transport reports: a status that is not
		// retried at all (a setting or a credential), a connection that
		// did not open, a wait that was cut short. Nothing was asked
		// again, so nothing here promises that asking again would help.
		case strings.HasPrefix(cause, worker.TransportFailedPhrase):
			return "受付の AI (" + stage + ") への問い合わせが通りませんでした。" +
				"設定か接続の問題である可能性があり、同じ依頼を出し直しても同じ結果になることがあります。" +
				"運用担当者が原因を確認します。\n"
		case strings.HasPrefix(cause, worker.ShapeRefusedPhrase):
			return "受付の AI (" + stage + ") の答えが、決められた形になりませんでした。" +
				"聞き直しても同じでした。同じ依頼をそのまま出し直しても同じ結果になる可能性が高いです。" +
				"運用担当者が受付の設定を確認します。\n"
		}
	}
	return ""
}

// unnamedReceptionNote is what a requester is told when the worker's cause is
// one the runner has no words for. It is deliberately the last resort and not
// a silence: before it, such a failure left the terminal comment saying the
// failure class and nothing else (live 2026-09-09).
func unnamedReceptionNote(stage string) string {
	return "受付の AI (" + stage + ") が答えを返せなかったため、自動処理を止めました。" +
		"依頼の内容ではなく自動処理側の問題です。運用担当者が実行記録で理由を確認します。\n"
}

// receptionNote renders the requester-facing note for a step's stderr. Every
// reception failure gets one: the specific notes first, then the causes the
// worker names, and last the note that says only that the stage could not
// answer.
func receptionNote(stage, stderr string) string {
	if note := receptionCutoffNote(stage, stderr); note != "" {
		return note
	}
	if note := receptionCauseNote(stage, stderr); note != "" {
		return note
	}
	return unnamedReceptionNote(stage)
}

// receptionCutoffNote renders the requester-facing note for a step's stderr,
// or "" when the step did not fail for a reason the requester can be told.
func receptionCutoffNote(stage, stderr string) string {
	if stage == deriveStage && noFileChosen(stderr) {
		return "この依頼で変更するファイルを決められなかったため、自動処理を止めました (" + stage + ")。" +
			"依頼に書かれたファイルがリポジトリに見つからず、依頼文からも新しく作るファイルの名前を読み取れなかった場合に起きます。" +
			"依頼文に、変更するファイルの位置を書き足して出し直してください (例: docs/ の下に新しく作る場合は、その相対パスをそのまま書く)。\n"
	}
	// Read at the same fixed position as every other cause. Scanning the
	// whole output for the marker let a requester choose this note and
	// which of its three forms they were shown: a failure the model itself
	// refused carries the head of its answer, and a ticket's own words
	// reach that answer (measured, review of #122).
	cutoff := receptionCauseOf(stderr, worker.CutoffPhrase)
	if !strings.Contains(cutoff, receptionCutoffMarker) {
		return ""
	}
	note := "受付の AI (" + stage + ") の答えが長すぎて出力の上限で途切れたため、自動処理を止めました。"
	switch {
	case strings.Contains(cutoff, worker.CutoffAskedAgainPhrase):
		note += "上限を広げて 1 回聞き直しましたが、それでも途切れました。"
	case strings.Contains(cutoff, worker.CutoffAtCeilingPhrase):
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
