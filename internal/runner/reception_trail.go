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

// The notes say what happened and what would help, and never tell the
// requester to do something: the comment they arrive in states that the
// requester need not act and that an operator will look at it, so an
// instruction here would hand them two opposite directions in one comment
// (review of #122). Making that line follow the note is its own change.
//
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
	_, cause, found := strings.Cut(strings.TrimPrefix(rest, workerLinePrefix), ":")
	if !found {
		return ""
	}
	return strings.TrimLeft(cause, " ")
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
func receptionCauseNote(stage, cause string) string {
	// The worker's own ending line is the last one it writes, and the lines
	// before it can be its own re-ask notices. Reading forwards would let a
	// transient the run recovered from decide the note ahead of what
	// actually ended the stage; the cutoff reader already read backwards, so
	// the two agree now (review of #122).
	{
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
				"同じ依頼をもう一度動かせば通る見込みです。\n"
		// A limit that waiting does not lift. An exhausted balance is one
		// of these, and telling its requester to send the ticket again
		// would send them round the same wall with nobody looking at the
		// balance.
		case strings.HasPrefix(cause, worker.TransportFailedPhrase) &&
			(strings.Contains(cause, worker.LimitNotLiftedPhrase) || strings.Contains(cause, worker.RetryAfterTooLongPhrase)):
			return "受付の AI (" + stage + ") への問い合わせが、利用の上限に当たって断られました。" +
				"時間をおいて動かし直しても同じ結果になります。運用担当者が利用枠を確認します。\n"
		// Everything else the transport reports: a status that is not
		// retried at all (a setting or a credential), a connection that
		// did not open, a wait that was cut short. Nothing was asked
		// again, so nothing here promises that asking again would help.
		case strings.HasPrefix(cause, worker.TransportFailedPhrase):
			return "受付の AI (" + stage + ") への問い合わせが通りませんでした。" +
				"設定か接続の問題である可能性があり、同じ依頼を動かし直しても同じ結果になることがあります。" +
				"運用担当者が原因を確認します。\n"
		// The model declined over what it was asked. The ticket's own words
		// are in that question, so this is the one reception failure worth
		// telling its requester to look at their own wording for.
		case strings.HasPrefix(cause, worker.DeclinedOverContentPhrase):
			return "受付の AI (" + stage + ") が、依頼文の内容を理由に答えを断りました。" +
				"聞き直しても同じでした。依頼文の書き方を変えれば通る見込みです。\n"
		// The gateway's accounting, not the answer: a transient worth
		// sending the same ticket again for. Told as the opposite before
		// (review of #122).
		case strings.HasPrefix(cause, worker.GatewayBookkeepingPhrase):
			return "受付の AI (" + stage + ") との通信の記録が壊れていたため、答えを受け取れませんでした。" +
				"聞き直しても同じでした。一時的なことが多いため、同じ依頼をもう一度動かせば通る見込みです。\n"
		case strings.HasPrefix(cause, worker.AnswerUnusablePhrase):
			return "受付の AI (" + stage + ") の答えが、決められた形になりませんでした。" +
				"聞き直しても同じでした。同じ依頼を動かし直しても同じ結果になる可能性が高いです。" +
				"運用担当者が受付の設定を確認します。\n"
		}
	}
	return ""
}

// lastReceptionCause is the cause on the last line the worker wrote about
// its own failure, or "" when it wrote none.
func lastReceptionCause(stderr string) string {
	found := ""
	for _, line := range strings.Split(stderr, "\n") {
		if cause := receptionErrorText(line); cause != "" {
			found = cause
		}
	}
	return found
}

// unnamedReceptionNote is what a requester is told when the worker's cause is
// one the runner has no words for. It is deliberately the last resort and not
// a silence: before it, such a failure left the terminal comment saying the
// failure class and nothing else (live 2026-09-09).
func unnamedReceptionNote(stage string) string {
	// Asserts neither that a model was reached (some reception failures
	// happen before any call) nor that the ticket is blameless (a model may
	// refuse an answer over what the ticket asks for). Both were claimed
	// here and both are sometimes false (review of #122).
	// "受付処理 (<工程>)" rather than "受付の <工程>": two of the three stage
	// names already begin with 受付の, and this is the note a requester sees
	// most (review of #122). Not "受付の AI" either — this note is reached by
	// failures that never called a model.
	return "受付処理 (" + stage + ") が完了しなかったため、自動処理を止めました。" +
		"理由はこの記録からは特定できていません。運用担当者が実行記録で確認します。\n"
}

// receptionNote renders the requester-facing note for a step's stderr. Every
// reception failure gets one: the specific notes first, then the causes the
// worker names, and last the note that says only that the stage could not
// answer.
func receptionNote(stage, stderr string) string {
	// One line decides: the last one the worker wrote about its own failure,
	// which is the one that ended the stage. Asking each note in turn
	// whether its phrase appears anywhere let a cutoff the run recovered
	// from overrule the line that actually ended the stage (review of #122).
	cause := lastReceptionCause(stderr)
	if stage == deriveStage && strings.HasPrefix(cause, worker.NoTargetFileChosen) {
		return noFileChosenNote(stage)
	}
	if note := receptionCutoffNote(stage, cause); note != "" {
		return note
	}
	if note := receptionCauseNote(stage, cause); note != "" {
		return note
	}
	return unnamedReceptionNote(stage)
}

// receptionCutoffNote renders the requester-facing note for a step's stderr,
// or "" when the step did not fail for a reason the requester can be told.
func receptionCutoffNote(stage, cause string) string {
	if !strings.HasPrefix(cause, worker.CutoffPhrase) || !strings.Contains(cause, receptionCutoffMarker) {
		return ""
	}
	note := "受付の AI (" + stage + ") の答えが長すぎて出力の上限で途切れたため、自動処理を止めました。"
	switch {
	case strings.Contains(cause, worker.CutoffAskedAgainPhrase):
		note += "上限を広げて 1 回聞き直しましたが、それでも途切れました。"
	case strings.Contains(cause, worker.CutoffAtCeilingPhrase):
		note += "上限は既に最大値だったため、聞き直しはできませんでした。"
	}
	note += "同じ依頼を動かし直しても同じ結果になる可能性が高いです。運用担当者が受付モデルの出力上限を確認します。\n"
	return note
}

// noFileChosenNote is what a requester is told when the derivation had no
// file to change: the one reception failure whose remedy is in the ticket.
func noFileChosenNote(stage string) string {
	return "この依頼で変更するファイルを決められなかったため、自動処理を止めました (" + stage + ")。" +
		"依頼に書かれたファイルがリポジトリに見つからず、依頼文からも新しく作るファイルの名前を読み取れなかった場合に起きます。" +
		"依頼文に、変更するファイルの位置 (例: docs/ の下に新しく作るなら、その相対パス) を書き足せば通る見込みです。\n"
}

// noteReceptionRecord leaves the requester a reason for a reception stage
// that ended over a record the pipeline could not read or could not accept.
// No model is involved in reading a record, so the notes above have nothing
// to say about it, and without this the ticket carried the failure class and
// nothing else — five of the readiness gate's seven such exits (review of
// #122). Best-effort, like the note above: an unwritable trail must not
// change the outcome.
func (p *Pipeline) noteReceptionRecord(stage string) {
	note := "受付処理 (" + stage + ") の記録を読めなかったため、自動処理を止めました。" +
		"依頼の内容とは別のところで止まっています。運用担当者が記録を確認します。\n"
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
