package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The implementing agent is told that when it cannot carry out the request,
// it must change nothing, say why, and leave the decision to the requester.
// It did exactly that on a live run (2026-09-25) and the engine threw the
// answer away: the empty working copy went to the first review card, the
// seal refused it as "the agent changed nothing", and the run ended as
// model_failed with a terminal comment that carried neither the refusal nor
// its reason. The requester was told the AI had failed; the AI had in fact
// answered.
//
// Returned work is not a failure of the automation, and it is no longer an
// ending either. The round stops before the review — an empty working copy
// has nothing to judge — and then the engine answers the report itself and
// starts the same round again. What the agent said, and what was decided in
// its place, is recorded beside the round and reaches the requester as part
// of the result rather than instead of one.

// implementingRunRecords are the run records an implementation round may
// leave, in the order they are looked for: the card names its record for
// the role that ran, and the seal that follows writes the implement verb's
// own record beside it.
var implementingRunRecords = []string{"implementer-run.json", "applier-run.json", "implement-run.json"}

// EmptyAttemptRecordPath is where the launch that reported work and changed
// nothing is kept: beside the run record, under a name nothing else writes.
//
// Named here rather than where it is written because two processes want it.
// The verb writes it, and the card that starts the same round again has to
// clear it first: the record is exclusive-create, so a leftover would make
// every later attempt's account of itself vanish silently.
func EmptyAttemptRecordPath(runOut string) string {
	extension := filepath.Ext(runOut)
	return strings.TrimSuffix(runOut, extension) + "-empty-attempt" + extension
}

// IsSendBack reports whether an implementing run returned the work instead
// of doing it: it finished, it changed nothing, and it said something. The
// working tree is what decides "changed nothing" — the same measurement the
// empty-result retry already makes — and the transcript is what separates a
// refusal with a reason from an agent that produced nothing at all, which is
// still a model failure.
func IsSendBack(run AgentRun) bool {
	return run.ExitCode == 0 && len(run.ChangedFiles) == 0 && strings.TrimSpace(run.Transcript) != ""
}

// ReadImplementingRun reads a round's implementing run record, whichever
// role wrote it.
//
// The record is deliberately not put through AgentRun.Validate: that refuses
// an implementing run which changed no files, which is the very record this
// function exists to read. Its fields were bounded by the seal that wrote
// them, and nothing here acts on the record beyond reading what the agent
// said and whether it changed anything.
func ReadImplementingRun(historyDir string, round int) (AgentRun, error) {
	stageDir := filepath.Join(historyDir, "stage-"+strconv.Itoa(round))
	for _, name := range implementingRunRecords {
		var run AgentRun
		if err := ReadJSONFile(filepath.Join(stageDir, name), MaxArtifactJSONBytes, &run); err != nil {
			continue
		}
		return run, nil
	}
	return AgentRun{}, errors.New("the round left no implementing run record")
}

// RoundReturnedWork reports whether a round's implementing agent returned
// the work to the requester. A round with no readable record did not — an
// unreadable record is the machinery's own failure and keeps the ending it
// always had — and neither does one that got as far as a candidate, whose
// agent plainly did change something.
func RoundReturnedWork(historyDir string, round int) bool {
	if candidateSealedFor(historyDir, round) {
		return false
	}
	run, err := ReadImplementingRun(historyDir, round)
	return err == nil && IsSendBack(run)
}

// candidateSealedFor reports whether a round fixed its changes into a
// candidate. Its absence is what makes a round's run record the only record
// of what happened.
func candidateSealedFor(historyDir string, round int) bool {
	info, err := os.Stat(filepath.Join(historyDir, "stage-"+strconv.Itoa(round), "candidate.json"))
	return err == nil && info.Mode().IsRegular()
}

// ReportText is an agent's own words, made safe to carry in a
// requester-facing record: the characters a trail may not hold are removed
// and the ends are trimmed. Nothing is shortened here — the trail's own
// bound is the only limit, so the whole report travels as far as that
// allows. It is introduced as the agent's report wherever it is rendered,
// so it is never read as the engine's own statement.
func ReportText(transcript string) string {
	var cleaned strings.Builder
	cleaned.Grow(len(transcript))
	for _, r := range transcript {
		if r == '\n' || r == '\t' {
			cleaned.WriteRune(r)
			continue
		}
		// A carriage return is dropped rather than turned into a newline:
		// the common case is a CRLF line ending, and translating it would
		// double every line break in the report.
		if r < 0x20 || r == 0x7f {
			continue
		}
		cleaned.WriteRune(r)
	}
	return strings.TrimSpace(cleaned.String())
}

// What happens next is the engine's own decision, not the requester's.
//
// The agent's report is still read the same way; what changed is where it
// goes. A delivery that stops at midnight to ask whether to use a default,
// or to be handed a key, is a delivery that is not finished in the morning,
// and the requester asked for the opposite of that. So after reception
// nothing is handed back: the round is answered here and started again.
//
// Two things a returned report can be short of, and one answer to each.
// Information the ticket did not settle is answered with the reading that
// is easiest to defend — the request and the working copy decide it, and
// what was decided is recorded as an assumption so the report can say it
// out loud. A key, or a way into something outside the run, is answered
// with a stand-in: the change is built against a test double so the
// destination's own commands can still judge it, and what has to be
// supplied for the real thing is recorded beside it. Neither answer is a
// question.

const (
	// AssumptionImplementerReturn is the reading the engine put in place of
	// what a returned report said was missing.
	AssumptionImplementerReturn = "implementer_return"
	// AssumptionCredentialSubstituted is a stand-in put where a key or a
	// way into an outside service was asked for, with what must be supplied
	// for the real thing recorded beside it.
	AssumptionCredentialSubstituted = "credential_substituted"
)

// ReturnRecordFileName is where one round's returns are kept, beside that
// round's other records.
const ReturnRecordFileName = "returns.json"

// ReturnRecordSchemaVersion is this record's shape.
const ReturnRecordSchemaVersion = ArtifactSchemaVersion

const (
	// maxReturnedReportBytes bounds the report carried in the record. The
	// whole report reaches the requester through the trail, which has its
	// own budget; what is kept here is what the next attempt is shown of
	// its own previous answer, and that rides inside a bounded prompt.
	maxReturnedReportBytes = 8 * 1024
	// maxReturnAttempts bounds how many returns one round records. It is a
	// bound on the file, not on the delivery; what bounds the answering is
	// maxAnsweredReturns below.
	maxReturnAttempts = 64
	// maxAnsweredReturns is how many times the engine answers one round
	// itself before it stops calling the return an answerable one.
	//
	// The engine's answer does not vary: it is the same three rules every
	// time. An implementer that has been given them once and comes back
	// anyway is not going to be moved by being given them a second time,
	// and a third is paying for a whole agent launch to find that out. Past
	// this the return is read as what it has become — the seat's model
	// declining the work after being told there is nowhere to hand it back
	// — and the remedy is the ladder's: a shorter instruction, a different
	// occupant, a wait. Three because the first answer is the one worth
	// making, and two more cover a report that names something genuinely
	// new each time.
	maxAnsweredReturns = 3
	// maxReturnSupplies and maxReturnSupplyBytes bound the lines quoted out
	// of a report as what has to be supplied. They are the agent's own
	// words about what it was missing, not the engine's summary of them.
	maxReturnSupplies    = 8
	maxReturnSupplyBytes = 500
)

// ReturnedWork is one answer to one returned round: what the agent said,
// what the engine decided in its place, and what the same round is being
// told to do now.
type ReturnedWork struct {
	Attempt  int  `json:"attempt"`
	Repeated bool `json:"repeated"`
	// Answered is whether the engine answered this return itself. False
	// means the round was not started again over it: the return repeated
	// one already answered, or the round had used up the answers the engine
	// makes, and what happens to it instead is the ladder's business. The
	// assumption and the instruction are empty for such a return — nothing
	// was decided and nothing was said to anyone.
	Answered bool `json:"answered"`
	// RunSHA256 identifies the launch this return was read from, which is
	// what tells one return from the next when the words are the same. Two
	// launches never share it: the digest covers when the agent ran.
	//
	// It exists because a card the ladder is waiting on stays blocked, and
	// the tick looks at a blocked card every few seconds. Without a way to
	// recognise the launch, every one of those ticks would read the same
	// record as a fresh return and write the round's history again.
	RunSHA256    string              `json:"run_sha256,omitempty"`
	ReportSHA256 string              `json:"report_sha256"`
	Report       string              `json:"report"`
	Supply       []string            `json:"supply,omitempty"`
	Assumption   ReadinessAssumption `json:"assumption,omitempty"`
	Instruction  string              `json:"instruction,omitempty"`
	AnsweredAt   time.Time           `json:"answered_at"`
}

// ReturnedRound is every return one round has made, oldest first.
type ReturnedRound struct {
	SchemaVersion int            `json:"schema_version"`
	Stage         int            `json:"stage"`
	Returns       []ReturnedWork `json:"returns"`
}

// Latest is the answer the round is running under now, or nil for a round
// that has never been handed back.
func (r *ReturnedRound) Latest() *ReturnedWork {
	if r == nil || len(r.Returns) == 0 {
		return nil
	}
	return &r.Returns[len(r.Returns)-1]
}

// AlreadyLeftToTheLadder reports whether this round's newest return is one
// the engine has already declined to answer, read from the same launch.
//
// The card it failed on stays blocked while the ladder works on it, and the
// tick looks at a blocked card every few seconds. Each of those ticks reads
// the round's records again and finds exactly what the tick before it
// found: no agent has run, so there is no new return. Recording it again
// would rewrite the round's history every few seconds for as long as the
// ladder's longest wait, and would count ticks as attempts — which is the
// number that decides how many times the engine answers.
func (r *ReturnedRound) AlreadyLeftToTheLadder(run AgentRun) bool {
	last := r.Latest()
	return last != nil && !last.Answered && last.RunSHA256 == run.RunSHA256
}

// Assumptions is what the engine decided across this round's returns, in
// the order it decided them, for the record the requester reads. A return
// the engine did not answer decided nothing and contributes nothing.
func (r *ReturnedRound) Assumptions() []ReadinessAssumption {
	if r == nil {
		return nil
	}
	assumptions := make([]ReadinessAssumption, 0, len(r.Returns))
	for _, returned := range r.Returns {
		if returned.Answered {
			assumptions = append(assumptions, returned.Assumption)
		}
	}
	return assumptions
}

// AnswerReturn reads one return and decides what becomes of it: an answer
// the engine makes itself, or a return it will not answer, which its caller
// hands to the ladder.
//
// The engine writes the answer itself rather than asking a model. The
// decision does not vary: the work is never handed back, missing
// information takes the most defensible reading, and a key becomes a
// stand-in. A model would be paid to restate a rule that is already written
// down, once per return, inside the attendant's own loop — and a returned
// round has neither a sealed candidate nor a sealed review, which is what
// the arbiter seat reads. The arbiter rules on rounds that produced
// something and disagreed about it; this is a round that produced nothing,
// and the answer to it is policy rather than judgement.
//
// Two returns are not answered. One that repeats a report already answered,
// at any attempt, because the answer has plainly not moved the implementer
// and repeating it would only cost another launch. And one past
// maxAnsweredReturns, because by then the same is true however the wording
// varied. Both are the seat's model declining the work, and the ladder is
// what this engine does about a model that will not answer.
func AnswerReturn(run AgentRun, previous *ReturnedRound, answeredAt time.Time) ReturnedWork {
	report := boundedReport(ReportText(run.Transcript))
	digest := sha256.Sum256([]byte(report))
	returned := ReturnedWork{
		Attempt:      1,
		RunSHA256:    run.RunSHA256,
		ReportSHA256: hex.EncodeToString(digest[:]),
		Report:       report,
		Supply:       returnSupplies(report),
		AnsweredAt:   answeredAt,
	}
	if last := previous.Latest(); last != nil {
		returned.Attempt = last.Attempt + 1
		// The same words again, compared against the newest return rather
		// than against any of them: what matters is whether the answer this
		// round is running under changed anything, and that is the one
		// before this.
		returned.Repeated = last.ReportSHA256 == returned.ReportSHA256
	}
	returned.Answered = !returned.Repeated && returned.Attempt <= maxAnsweredReturns
	if returned.Answered {
		returned.Assumption = returnAssumption(returned)
		returned.Instruction = returnInstruction(returned)
	}
	return returned
}

// boundedReport keeps a report inside the record's budget, cut on a rune
// boundary and said to have been cut. The trail carries the whole thing; a
// report longer than this has already said why it stopped in its opening
// lines.
func boundedReport(report string) string {
	if len(report) <= maxReturnedReportBytes {
		return report
	}
	const notice = "\n(報告が長いため、ここまでを記録しています)"
	kept := report[:maxReturnedReportBytes-len(notice)]
	for len(kept) > 0 && !utf8.ValidString(kept) {
		kept = kept[:len(kept)-1]
	}
	return strings.TrimSpace(kept) + notice
}

// returnSupplyMarkers are the words a report uses when what it was missing
// is a key or a way into something outside the run.
//
// A plain word match, over a report that has already been through
// ReportText. It is deliberately loose in one direction only: a line that
// matches but meant something else costs one quoted line and a differently
// named assumption, while a line that does not match still gets the
// stand-in, because the instruction states that rule for every return. So
// there is no reading of a report that sends the work back.
var returnSupplyMarkers = []string{
	"api key", "api_key", "apikey", "access key", "access token",
	"secret", "credential", "password", "oauth",
	"資格情報", "認証情報", "アクセストークン", "トークン", "パスワード",
	"秘密鍵", "api キー", "apiキー", "鍵",
}

// returnSupplies is what the report itself named as missing, in its own
// words: the lines that mentioned a key or a way in, trimmed and bounded.
func returnSupplies(report string) []string {
	supplies := make([]string, 0, maxReturnSupplies)
	for _, line := range strings.Split(report, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		folded := strings.ToLower(trimmed)
		named := false
		for _, marker := range returnSupplyMarkers {
			if strings.Contains(folded, marker) {
				named = true
				break
			}
		}
		if !named {
			continue
		}
		supplies = append(supplies, boundedSupplyLine(trimmed))
		if len(supplies) == maxReturnSupplies {
			break
		}
	}
	if len(supplies) == 0 {
		return nil
	}
	return supplies
}

func boundedSupplyLine(line string) string {
	if len(line) <= maxReturnSupplyBytes {
		return line
	}
	kept := line[:maxReturnSupplyBytes]
	for len(kept) > 0 && !utf8.ValidString(kept) {
		kept = kept[:len(kept)-1]
	}
	return strings.TrimSpace(kept)
}

// returnAssumption is what the engine put in place of what the report said
// it lacked, in the terms the requester's own report is built from.
func returnAssumption(returned ReturnedWork) ReadinessAssumption {
	evidence := fmt.Sprintf("実装役の報告 (%d 回目、報告の指紋 %s)", returned.Attempt, shortDigest(returned.ReportSHA256))
	if len(returned.Supply) > 0 {
		return ReadinessAssumption{
			Kind:      AssumptionCredentialSubstituted,
			Statement: "鍵や外部サービスへの接続が要ると報告された点は、本物を使わずに代役を実装して検証を通し、本物として供給すべきものを報告に残す。",
			Evidence:  evidence + " が挙げた不足: " + strings.Join(returned.Supply, " / "),
		}
	}
	return ReadinessAssumption{
		Kind:      AssumptionImplementerReturn,
		Statement: "情報が足りないと報告された点は、依頼と作業コピーの中で最も擁護できる既定を採用して進める。",
		Evidence:  evidence,
	}
}

// shortDigest is a digest at the length a person reads it at.
func shortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// returnInstruction is what the same round is told, in the requester's
// language, as a requirement rather than an option.
func returnInstruction(returned ReturnedWork) string {
	lines := []string{
		"前の実行では、作業コピーを変更せずに理由だけを報告して終えました。この自動化に作業を返す先はありません。同じ巡をもう一度実行しますので、今回は次のとおり進めてください。",
		"- 依頼に書かれていない点は、依頼と作業コピーの中で最も擁護できる既定を自分で選び、選んだ理由を最後の報告に書いてください。決められないことを理由に中断しないでください。",
		"- 鍵・資格情報・外部サービスへの接続が要る点は、本物を使わずに代役 (test double / fake) を実装し、決められた検証が通る状態にしてください。本物として何を供給すべきかは最後の報告に書いてください。運用担当者がそれを読んで用意します。",
		"- 変更を 1 つも加えずに終了することはできません。",
	}
	if len(returned.Supply) > 0 {
		lines = append(lines, "- 前の実行が不足として挙げたもの: "+strings.Join(returned.Supply, " / "))
	}
	// Where the round stands, for a round that has come back before. The
	// count is the honest thing to say: this is the last of the answers the
	// engine makes, or the one before it, and after them the round is
	// handled as a model that will not do the work.
	if returned.Attempt > 1 {
		lines = append(lines, fmt.Sprintf(
			"- この巡が戻ってきたのは %d 回目です。本体が指示を出し直すのは %d 回までで、それを過ぎた分と、同じ報告の繰り返しは、実装役が作業を引き受けなかったものとして扱います。",
			returned.Attempt, maxAnsweredReturns))
	}
	return strings.Join(lines, "\n")
}

// ReadReturnedRoundFile reads back a round's returns. A round nobody handed
// back — nearly all of them — reads as no record and no error.
//
// A file that is there and will not read is an error rather than an
// absence. Read as absent, the round would be answered as though it had
// been handed back for the first time, and a report that had already been
// answered once would be answered the same way again forever.
func ReadReturnedRoundFile(path string) (*ReturnedRound, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.New("returned-work record could not be read")
	}
	var record ReturnedRound
	if err := ReadJSONFile(path, MaxArtifactJSONBytes, &record); err != nil {
		return nil, errors.New("returned-work record could not be read")
	}
	if record.SchemaVersion != ReturnRecordSchemaVersion {
		return nil, errors.New("returned-work record is of another shape")
	}
	return &record, nil
}

// Append adds one answer to the round's record.
//
// At the bound the record keeps its opening returns and its newest one, and
// thins what is between them. The opening is what the report quotes — it is
// where the engine started deciding for the agent — and the newest is what
// the next return is compared against, so neither may be dropped. A round
// handed back sixty-four times is saying nothing new in the middle.
func (r *ReturnedRound) Append(stage int, returned ReturnedWork) {
	r.SchemaVersion = ReturnRecordSchemaVersion
	r.Stage = stage
	if len(r.Returns) >= maxReturnAttempts {
		r.Returns = r.Returns[:maxReturnAttempts-1]
	}
	r.Returns = append(r.Returns, returned)
}
