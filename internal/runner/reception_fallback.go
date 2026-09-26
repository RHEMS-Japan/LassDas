package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/worker"
)

// The reception, read without the model.
//
// The reception is the only place this engine asks the requester anything,
// and it is not on the ladder: a seat that will not answer there has no
// other seat to be moved to, no shorter question to be asked, and nothing a
// wait would change. So for a long time the reception was the one stage
// whose model could end a delivery — and it ended three of them in two
// minutes each, on requests that genuinely lacked information, because the
// reader named a field with a word outside the shape the engine held it to.
// The requests the single round of questions exists for were the requests
// it killed.
//
// Reading a model's answer for what is needed rather than for its shape
// (internal/modeljson) is most of the answer. This is the rest of it: when
// every answer a reception reader gives is unusable, the engine makes the
// reading it can make on its own and the delivery goes on. The request is
// the ticket's own text; nothing is read out of it; no question is put to
// anybody. The implementer reads the destination's repository itself, which
// is where the rest of what the reader would have supplied actually is.
//
// The requester is told, once. The line goes in the run's stream of
// decisions, which both the implementation-plan notice and the closing
// comment already read, so it appears in the two places a requester meets
// what the engine settled on their behalf — and appears once even when both
// halves of the reception had to be read this way.

// receptionFallbackKind is the kind the line is recorded under. The stream's
// reader does not need to know it: a kind it has never heard of reads as an
// assumption and is listed as one, which is what this is.
const receptionFallbackKind = "reception_unreadable"

// receptionFallbackStatement is what the requester reads. It says what the
// engine did and what follows from it, in that order, and names no file, no
// model and no field.
const receptionFallbackStatement = "受付の読み取り役が読める形で答えなかったため、依頼はチケットの本文をそのまま実装役へ渡し、" +
	"受付からの質問はしていません。何を変えるかは実装役がリポジトリを読んで決めます。"

// assumptionsStreamFile is the run's stream of decisions, one JSON object
// per line, read by the plan notice and the closing comment.
const assumptionsStreamFile = "history/assumptions.jsonl"

// recordReceptionFallback appends the line, once per delivery.
//
// A failure to append is logged and nothing else: the delivery continuing is
// the point of this path, and a stream that could not be written must not be
// the thing that ends it. What the requester then loses is the line, not the
// work — and the closing comment says which records it could not read.
func (p *Pipeline) recordReceptionFallback() {
	if err := appendReceptionFallback(p.Workspace); err != nil {
		p.Logger.Error("the reception was read without the model and the note could not be written",
			"error", err.Error())
	}
}

func appendReceptionFallback(workspace string) error {
	if workspace == "" {
		return errors.New("the run directory is not known")
	}
	path := filepath.Join(workspace, assumptionsStreamFile)
	recorded, err := receptionFallbackRecorded(path)
	if err != nil {
		return err
	}
	if recorded {
		return nil
	}
	encoded, err := json.Marshal(runAssumption{
		Kind: receptionFallbackKind, Statement: receptionFallbackStatement,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// receptionFallbackRecorded reports whether this delivery already carries the
// line. The whole stream is read rather than a marker file kept beside it:
// the stream is the record, and a marker could disagree with it.
func receptionFallbackRecorded(path string) (bool, error) {
	encoded, err := readWorkspaceFile(path, maxOutcomeArtifactBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(encoded), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var assumption runAssumption
		if json.Unmarshal([]byte(line), &assumption) != nil {
			continue
		}
		if assumption.Kind == receptionFallbackKind {
			return true, nil
		}
	}
	return false, nil
}

// decideWithoutReaders answers whether a reception model stage that failed
// failed over the shape of an answer, and seals the gate itself when it did.
//
// The distinction it draws is the whole safety of this path. A reader whose
// answers could not be used has told the engine nothing, and there is
// nothing further to try: no other seat, no shorter question, no wait. A
// reader that could not be reached at all — a key with nothing left to
// spend, a provider that refused, a network that is not there, a volume with
// no room — is an instance an operator has to change, and a gate decided
// without readers would hide that behind a delivery that went ahead. So only
// the first takes this path; the second ends the way it always did.
//
// The worker says which it was on a line of its own, in its own words, from
// fields a model's answer cannot write into.
func (p *Pipeline) decideWithoutReaders(ctx context.Context, stage string) (Outcome, bool) {
	detail, spoke := worker.ParseFailureDetailLine(p.lastStepStderr)
	if !spoke || !strings.Contains(detail.Phrase, worker.AnswerUnusablePhrase) {
		return Outcome{}, false
	}
	// Kept as evidence even though the delivery goes on: the turn did fail,
	// and the numbers behind it are what an operator reads to see which
	// reader is answering badly and how often.
	p.recordModelFailureDetail(stage)
	decision := p.path("history/readiness/decision.json")
	if err := os.MkdirAll(filepath.Dir(decision), 0o755); err != nil {
		p.Logger.Error("the gate could not be decided without its readers", "error", err.Error())
		return Outcome{}, false
	}
	// Removed first: the failed stage may have left a partial record at the
	// path, and the seal below refuses to write over one.
	if err := os.Remove(decision); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.Logger.Error("the gate could not be decided without its readers", "error", err.Error())
		return Outcome{}, false
	}
	if code, err := p.worker(ctx, "decide-readiness-without-readers", []string{
		"decide-readiness-without-readers", "--config", p.Config.ConsumerConfigPath,
		"--tool-sha", p.Config.Identity.EngineSHA,
		"--ticket", p.path("readiness-ticket.json"), "--source", p.path("readiness-source.json"),
		"--out", decision,
	}); err != nil || code != 0 {
		// The gate could not be sealed, so there is no gate: the delivery
		// ends the honest way rather than going on past a record that is not
		// there.
		p.Logger.Error("the gate could not be decided without its readers", "stage", stage)
		return Outcome{}, false
	}
	p.recordReceptionFallback()
	p.Logger.Info("the reception's reader answered nothing usable; the gate was decided without it",
		"stage", stage)
	return Outcome{}, true
}
