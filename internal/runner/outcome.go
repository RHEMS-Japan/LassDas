package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

// What a person wants out of a night's work is the result of it.
//
// The closing comment used to open with one fixed sentence about the kind of
// ending, then a run link, then a pull request, then a round-by-round record
// of who objected to what. Every part of that is true and none of it answers
// the first question: what can I do now that I could not do last night, and
// where do I look at it. A requester reading the record has to reconstruct
// the answer from the account of the work, which is the wrong way round.
//
// So the run composes the answer itself, out of its own sealed records, and
// the report carries it as text. Two pieces travel: what was delivered and
// where it can be seen, and everything the engine settled without asking.
// The second exists because nothing asks the requester anything after the
// reception: a point decided for them at two in the morning reaches them
// here or nowhere.
//
// A run that did not finish has as much to say here as one that did. The
// worst version of this comment was posted by a delivery whose pod was
// replaced mid-round: the record could not be composed, the fallback line
// said so, and that fallback line was the entire comment. What happened and
// what the engine did about it were both written down in the run directory
// at the time; nothing read them.

const (
	// maxOutcomeRounds bounds how far the composer looks for a round's
	// records. It is a fixed number rather than the destination's configured
	// round limit on purpose: a configuration that was raised or lowered
	// under a running delivery must not decide which of that delivery's own
	// records the report is allowed to read.
	maxOutcomeRounds = 50

	// outcomeItemRunes bounds one line of a list.
	outcomeItemRunes = 200

	// maxTriedRemedies bounds the remedies one failed step lists. The ladder
	// has a handful of distinct hands and each is named once however often
	// it was played, so this is a guard rather than a cut anything reaches.
	maxTriedRemedies = 8

	// The two carriers of what the engine decided have very different room,
	// and every decision has to land in both.
	//
	// The pull request description takes the whole list: it has tens of
	// kilobytes and it is the one place a person can read every decision, so
	// its bound is set where no real delivery reaches it and the list is
	// held whole in practice. The ticket comment takes what one comment
	// holds — no count of its own, because a fixed count of eight dropped
	// decisions a shorter list would have had room for, and the byte budget
	// is the only honest limit. What either one cannot carry is named, and
	// named by pointing somewhere that really holds it.
	deliveryListItems       = 200
	deliveryAssumptionBytes = 32 * 1024
	// commentListItems is no limit at all: the comment's list is bounded by
	// bytes alone.
	commentListItems = 1 << 30

	// outcomeProseRunes bounds the request read back as what was delivered.
	// It is a paragraph, not the ticket.
	outcomeProseRunes = 400

	// maxOutcomeArtifactBytes bounds each record read. These are sealed JSON
	// documents of a few kilobytes; anything larger is a wrong file, and a
	// report must never be the thing that reads an unbounded file.
	maxOutcomeArtifactBytes = 1 << 20
)

// runAssumption is one thing the engine settled on its own, in the shape
// every producer of one seals it in: what kind of decision it was, what was
// decided, and on what evidence.
type runAssumption struct {
	Kind      string `json:"kind"`
	Statement string `json:"statement"`
	Evidence  string `json:"evidence"`
}

// The kinds a decision is recorded under. They are spelled here rather than
// imported because the packages that write them are not all merged yet and
// this reader must work before and after each of them arrives: a kind it has
// never heard of still reads as a decision, and is listed as one.
const (
	assumptionDefensibleDefault = "defensible_default"
	assumptionArbiterRuling     = "arbiter_ruling"
	assumptionCredentialStandIn = "credential_substituted"
)

// outcomeNotes collects the records this report could not read.
//
// Every reader here is forgiving, and has to be: a delivery that finished
// must not be unable to say so because one file went bad. But a section
// that is silently absent reads as "nothing of that kind happened", which
// is a different claim from "this could not be read" — and the first is a
// claim this report has no business making on the second's evidence. So a
// record that is there and will not read is named.
//
// A record that is simply not there is the ordinary case and says nothing:
// most rounds are never ruled on, most deliveries create no resources.
type outcomeNotes struct{ unreadable []string }

func (n *outcomeNotes) failed(kind string, err error) {
	if n == nil || err == nil || errors.Is(err, fs.ErrNotExist) {
		return
	}
	if !slices.Contains(n.unreadable, kind) {
		n.unreadable = append(n.unreadable, kind)
	}
}

// line is what the report says about them, in the section that never gives
// way. Which records, not how they broke: the requester's question is what
// is missing from what they are reading.
func (n *outcomeNotes) line() string {
	if n == nil || len(n.unreadable) == 0 {
		return ""
	}
	return "## 読み取れなかった記録\n次の記録はこの実行に残っていますが読み取れませんでした。" +
		"その分の内容はこの報告から抜けています: " + strings.Join(n.unreadable, "、") + "\n"
}

// The names those records go by on the ticket. They are the requester's
// words for what the record is about, not the file it lives in.
const (
	recordRequest   = "依頼の解釈"
	recordObserved  = "反映先の画面の確認結果"
	recordFailure   = "工程の失敗の記録"
	recordLadder    = "やり直しの記録"
	recordReception = "受付が決めたこと"
	recordDecisions = "実行中に決めたこと"
	recordRuling    = "レビューの裁定"
	recordReturned  = "実装役に返された仕事への答え"
	recordSeatMove  = "担当の入れ替え"
	recordResources = "作った資源"
	recordPath      = "リリース経路の記録"
)

// composeOutcome writes the two sections the report leads with, out of the
// run directory. Nothing here can fail the report: a record that is missing
// or unreadable leaves its part out and, when it was there, is named.
func composeOutcome(runDir string, code hook.TerminalCode, evidence map[string]string) (string, string) {
	notes := &outcomeNotes{}
	decided := composeAssumptionsText(runDir, evidence["pull_request_url"], notes)
	// The outcome is composed second so that it carries the note about
	// records neither section could read, including the decisions'.
	return composeOutcomeText(runDir, code, evidence, notes), decided
}

// composeOutcomeText says what the delivery made possible and where that can
// be seen — or, for a delivery that ended in a failure, what happened and
// what the engine tried about it.
func composeOutcomeText(runDir string, code hook.TerminalCode, evidence map[string]string, notes *outcomeNotes) string {
	var builder strings.Builder
	if request := outcomeRequest(runDir, notes); request != "" {
		builder.WriteString(outcomeRequestHeading(code, evidence) + "\n")
		builder.WriteString(request + "\n")
	}
	if where := outcomeWhereToSee(runDir, code, evidence, notes); where != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(where)
	}
	if unapplied := outcomeUnapplied(runDir, notes); unapplied != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(unapplied)
	}
	if happened := outcomeWhatHappened(runDir, code, evidence, notes); happened != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(happened)
	}
	if unreadable := notes.line(); unreadable != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(unreadable)
	}
	// The outcome never sends a reader to the description: what it carries
	// is the delivery's own account, which the description does not repeat.
	text, _ := boundOutcomeText(builder.String(), hook.MaxOutcomeTextBytes,
		"全文は、運用担当者が保管しているこの実行の記録にあります。")
	return text
}

// outcomeRequestHeading decides what the request is called at the top of the
// comment, and the choice is the whole difference between a report and a
// false completion.
//
// A delivery that got somewhere may say what the requester can now do. One
// that did not may not: a run whose implementation card was killed by a pod
// being replaced rendered 「この依頼でできるようになったこと」 above the
// request on one line and 「工程が完了しませんでした」 three lines below it,
// which is the exact shape this report exists to end. Everything that did
// not reach somewhere calls the request what it is — the thing that was
// asked for — and leaves saying what happened to the section that says it.
func outcomeRequestHeading(code hook.TerminalCode, evidence map[string]string) string {
	if reachedSomewhere(code, evidence) {
		return "## この依頼でできるようになったこと"
	}
	return "## お預かりした依頼"
}

// reachedSomewhere reports whether this ending put the change somewhere it
// is running and a person can go and look at it.
//
// A proposed change is not that. A pull request asks for the change to be
// made; until somebody merges it, nothing the requester asked for is
// happening anywhere, so a delivery that stopped at its proposal has made
// nothing possible yet — however successfully it stopped there. The same
// for a stop that arrived with only a pull request behind it, and for one
// that arrived before anything at all.
//
// So the test is a screen carrying the change: staging or production, which
// is exactly the evidence the report gate makes those depths bring. The
// depth the run names is deliberately not the test — a proposal-only
// delivery names one too. An investigation delivers a report rather than a
// change and reaches no environment by design.
func reachedSomewhere(code hook.TerminalCode, evidence map[string]string) bool {
	switch code {
	case hook.TerminalSuccess, hook.TerminalCancelled:
		return evidence["staging_evidence_url"] != "" || evidence["production_evidence_url"] != ""
	}
	return false
}

// outcomeRequest is the engine's own reading of what was asked for, written
// back as the thing that is now done. It comes from the reception's sealed
// ticket, and from the draft the reception was built from when the run never
// got as far as sealing one.
func outcomeRequest(runDir string, notes *outcomeNotes) string {
	var ticket struct {
		Request string `json:"request"`
	}
	err := readOutcomeArtifact(filepath.Join(runDir, "readiness-ticket.json"), &ticket)
	if err == nil {
		if request := clipRunes(ticket.Request, outcomeProseRunes); request != "" {
			return request
		}
	}
	var draft struct {
		Request string `json:"request"`
	}
	draftErr := readOutcomeArtifact(filepath.Join(runDir, "ticket-draft.json"), &draft)
	if draftErr == nil {
		return clipRunes(draft.Request, outcomeProseRunes)
	}
	// Only when neither could be read: a run that sealed the reception's
	// ticket has no draft to miss, and naming the one it never wrote would
	// report a fault that is not there.
	notes.failed(recordRequest, err)
	notes.failed(recordRequest, draftErr)
	return ""
}

// outcomeWhereToSee names the places the change can be looked at, and what
// the engine saw when it looked. The depth the run actually reached decides
// which places those are: a delivery that stopped at its proposal has no
// screen to point at, and says so rather than pointing at one it never
// touched.
//
// A stop is asked the same question, because a stop is no longer the same
// as nothing having happened. A requester can stop a delivery whose change
// is already merged, deployed and looked at, and sending them away without
// naming the environment they now have to go and see would be the worst
// version of this comment. A stop that reached nowhere carries no evidence
// and this writes nothing, which the stop's own sentence already covers.
//
// A delivery cut short by its own deadline is asked it for the same reason.
// It can run out of night with its change already on staging — the
// promotion is the phase most likely to keep failing — and a report that
// said twice that staging holds the change without once giving the screen
// would send its reader hunting for a URL the run had in hand.
func outcomeWhereToSee(runDir string, code hook.TerminalCode, evidence map[string]string, notes *outcomeNotes) string {
	switch code {
	case hook.TerminalSuccess, hook.TerminalCancelled, hook.TerminalInvestigated,
		hook.TerminalDeadlineReached:
	default:
		return ""
	}
	var lines []string
	if url := evidence["production_evidence_url"]; url != "" {
		lines = append(lines, "production確認先: "+url)
		if seen := observedLine(runDir, DeliverProductionReportFile, "本番", notes); seen != "" {
			lines = append(lines, seen)
		}
	}
	if url := evidence["staging_evidence_url"]; url != "" {
		lines = append(lines, "staging確認先: "+url)
		if seen := observedLine(runDir, DeliverStagingReportFile, "staging", notes); seen != "" {
			lines = append(lines, seen)
		}
	}
	if len(lines) == 0 {
		// The merge-and-look card of the older delivery path leaves its own
		// verdict, and a delivery that only proposed a change has neither.
		observed := e2eObservedLine(runDir, notes)
		switch {
		case observed != "":
			lines = append(lines, observed)
		case code == hook.TerminalInvestigated:
			// An investigation delivers a report, not a change, and the
			// report is posted on this ticket. Saying nothing here left the
			// one ending whose whole product is a document without a word
			// about where to read it.
			lines = append(lines, "調査の報告はこのチケットに掲示しました。計測を添付した場合は同じコメントに付いています。")
		case code == hook.TerminalSuccess:
			lines = append(lines, "まだ動いている場所はありません。提案した変更は、下の Pull Request でご確認ください。")
		default:
			// A stop with nothing behind it. Saying where to look would be
			// inventing a place; the stop's own sentence says what it left.
			return ""
		}
	}
	return "## どこで見られるか\n" + strings.Join(lines, "\n") + "\n"
}

// observedLine says, in one sentence, what the observation of one phase
// actually saw. A phase that passed without looking at a screen says that
// too: a pass the engine never verified with its eyes must not read as one
// it did.
func observedLine(runDir, file, place string, notes *outcomeNotes) string {
	report, found, err := ReadDeliverReport(runDir, file)
	notes.failed(recordObserved, err)
	if !found {
		return ""
	}
	// A Latin-script name takes a space before a Japanese particle and a
	// Japanese one does not, so the spacing belongs to the name rather than
	// to each sentence built on it: without this the ticket read
	// 「stagingの画面」 here and 「staging の画面」 three lines below.
	if place != "" && place[len(place)-1] < 0x80 {
		place += " "
	}
	switch {
	case report.Verdict == "pass" && report.ScreenChecked && report.ExpectedText != "":
		return place + "の画面で「" + clipRunes(report.ExpectedText, 80) + "」が表示されているのを確認しました。"
	case report.Verdict == "pass" && report.ScreenChecked:
		return place + "の画面を開いて、表示に問題がないことを確認しました。"
	case report.Verdict == "pass":
		return place + "への反映を確認しました（この依頼は画面の文言を変えないため、表示の照合は行っていません）。"
	case report.Detail != "":
		return place + "の確認は通っていません: " + clipRunes(report.Detail, outcomeItemRunes)
	default:
		return ""
	}
}

// e2eObservedLine is the same for the observation card that watches a merge
// somebody else made. It is read only when no delivery phase sealed a report
// of its own, so the two can never both speak for the same screen.
func e2eObservedLine(runDir string, notes *outcomeNotes) string {
	result, found, err := ReadE2EResult(runDir)
	notes.failed(recordObserved, err)
	if !found {
		return ""
	}
	switch {
	case result.Verdict == "pass" && result.ExpectedText != "":
		return "staging の画面で「" + clipRunes(result.ExpectedText, 80) + "」が表示されているのを確認しました: " + result.TargetURL
	case result.Verdict == "pass":
		return "staging の画面を開いて、表示に問題がないことを確認しました: " + result.TargetURL
	case result.Detail != "":
		return "staging の確認は通っていません: " + clipRunes(result.Detail, outcomeItemRunes)
	default:
		return ""
	}
}

// outcomeWhatHappened is the account a delivery that did not finish owes its
// requester: which step stopped, what kind of thing went wrong, and what the
// engine did about it before it gave up. All of it was already written down
// in the run directory while it was happening.
func outcomeWhatHappened(runDir string, code hook.TerminalCode, evidence map[string]string, notes *outcomeNotes) string {
	if code == hook.TerminalSuccess || code == hook.TerminalInvestigated || code == hook.TerminalCancelled {
		return ""
	}
	failure, found := latestStageFailure(runDir, notes)
	step := evidence["failed_step"]
	ranOut := code == hook.TerminalDeadlineReached
	if !found && step == "" && !ranOut {
		return ""
	}
	var lines []string
	// The clock first, for the one ending it belongs to. What follows is
	// the failure the delivery was still working on when the time went, and
	// without this line above it the report would read as a delivery that
	// gave up on that failure rather than one that ran out of night.
	if ranOut {
		lines = append(lines, outcomeDeadlineSentence(evidence))
	}
	switch {
	case step != "" && found && failure.Round > 0:
		lines = append(lines, fmt.Sprintf("%s の工程が、%d 周目で完了しませんでした。", step, failure.Round))
	case step != "":
		lines = append(lines, step+" の工程が完了しませんでした。")
	case found || !ranOut:
		lines = append(lines, "工程のひとつが完了しませんでした。")
	}
	if found {
		handedBack := handedBackCount(runDir, failure.Round, notes)
		switch {
		case failure.Interrupted:
			lines = append(lines, "原因: 処理を動かしている場所が入れ替わり、工程が途中で止まりました。")
		case handedBack > 0:
			// The implementing agent answering rather than working seals an
			// ordinary model failure, so the class alone would report "the
			// AI did not answer" for a round where it answered every time
			// and refused every time. The round's own record of what was
			// handed back is the only thing that tells them apart.
			lines = append(lines, fmt.Sprintf("原因: AI が %d 回とも「この依頼はこのままでは実現できない」と作業を返してきました。", handedBack))
		default:
			if reason := failureClassSentence(failure.Class); reason != "" {
				lines = append(lines, "原因: "+reason)
			}
		}
	}
	text := "## 何が起きたか\n" + strings.Join(lines, "\n") + "\n"
	if tried := outcomeWhatWasTried(runDir, failure.Stage, failure.Round, notes); tried != "" {
		text += "\n" + tried
	}
	if ranOut {
		text += "\n" + outcomeReachedSoFar(evidence)
		if needed := outcomeWhatIsNeeded(runDir, failure, found, notes); needed != "" {
			text += "\n" + needed
		}
	}
	return text
}

// outcomeDeadlineSentence says the delivery ran out of the time it was
// given, and how much that was. The figure travels in the evidence because
// it is the operator's setting rather than anything the run sealed; a
// report from an engine that did not carry it keeps the sentence and drops
// the number, which is still the fact the requester needs.
func outcomeDeadlineSentence(evidence map[string]string) string {
	hours := evidence["deadline_hours"]
	if hours == "" {
		return "この依頼に使える処理時間を使い切ったため、ここで打ち切って、いまの状態をお知らせします。"
	}
	return "この依頼に使える処理時間 (" + hours + " 時間) を使い切ったため、ここで打ち切って、いまの状態をお知らせします。"
}

// outcomeReachedSoFar says what exists now, which for a delivery cut short
// is the question its requester asks first. It reads the same evidence the
// comment's links are built from, so the prose and the links cannot
// disagree about what landed.
func outcomeReachedSoFar(evidence map[string]string) string {
	var line string
	switch {
	case evidence["production_evidence_url"] != "":
		line = "本番環境への反映と確認までは完了しています。自動での巻き戻しは行っていません。"
	case evidence["staging_evidence_url"] != "":
		line = "staging への反映と確認までは完了しています。本番環境は変更していません。"
	case evidence["pull_request_url"] != "":
		line = "取り込み用の Pull Request は作成済みで、マージは行っていません。本番環境は変更していません。"
	default:
		line = "動いている場所はまだありません。Pull Request も作成していないため、対象リポジトリと本番環境は変更していません。"
	}
	return "## ここまでに出来上がっているもの\n" + line + "\n"
}

// outcomeWhatIsNeeded names the one thing that would let the same request
// go through next time. It is the half of an unfinished report that decides
// whether anybody can act on it: "the AI did not answer" is a fact, and
// "raise the key's limit" is a fact somebody can do something about.
func outcomeWhatIsNeeded(runDir string, failure StageFailure, found bool, notes *outcomeNotes) string {
	if !found {
		return ""
	}
	line := ""
	switch {
	case handedBackCount(runDir, failure.Round, notes) > 0:
		line = "AI が返してきた理由をこのチケットの記録でご確認のうえ、足りない情報を書き足して起票し直してください。"
	case failure.Class == FailureClassCredit:
		line = "AI の利用枠の上限を上げるか、枠のリセットを待ってから、同じ内容で起票し直してください。"
	case failure.Class == FailureClassNetwork:
		line = "外部との通信が回復していることを確認のうえ、同じ内容で起票し直してください。"
	case failure.Class == FailureClassTool:
		line = "作業に必要な道具が用意できる状態かを運用担当者にご確認のうえ、同じ内容で起票し直してください。"
	case failure.Class == FailureClassDisk:
		line = "作業用の保存領域を空ける必要があります。運用担当者の対応後、同じ内容で起票し直してください。"
	case failure.Class == FailureClassTimeout:
		line = "依頼の範囲を小さく分けて起票し直すと、同じ時間内で終わる見込みが上がります。"
	case failure.Class == FailureClassModel:
		line = "同じ内容で起票し直すと、別の AI と提供元で最初からやり直します。"
	default:
		return ""
	}
	return "## 続けるために必要なこと\n" + line + "\n"
}

// handedBackCount is how many times this round's implementing agent handed
// the work back instead of doing it. A round nobody handed back reads zero,
// which is the ordinary case.
func handedBackCount(runDir string, round int, notes *outcomeNotes) int {
	if round < 1 {
		return 0
	}
	record, err := ReadReturns(runDir, round)
	notes.failed(recordReturned, err)
	if record == nil {
		return 0
	}
	return len(record.Returns)
}

// failureClassSentence puts each kind of failure in words a requester can
// act on, or decide not to. The vocabulary is the one the remedies are
// organised by, so a person who reads two of these reports can tell whether
// the same thing keeps happening.
func failureClassSentence(class FailureClass) string {
	switch class {
	case FailureClassModel:
		return "AI が答えを返しませんでした。"
	case FailureClassNetwork:
		return "外部との通信が届きませんでした。"
	case FailureClassTool:
		return "作業に必要な道具が用意できませんでした。"
	case FailureClassValidation:
		return "作った変更が、このリポジトリの検証を通りませんでした。"
	case FailureClassDisk:
		return "作業用の保存領域が足りなくなりました。"
	case FailureClassCredit:
		return "AI の利用枠を使い切りました。枠を上げるか、リセットを待つ必要があります。"
	case FailureClassTimeout:
		return "工程が、与えられた時間内に終わりませんでした。"
	default:
		return ""
	}
}

// outcomeWhatWasTried lists the remedies the engine spent on the step that
// stopped. It is the half of the account that says the engine did not just
// sit there, and the half a person needs to decide whether to change
// anything before asking again.
func outcomeWhatWasTried(runDir, stage string, round int, notes *outcomeNotes) string {
	record, found := readLadderAttempts(runDir, stage, round, notes)
	if !found || (len(record.Tried) == 0 && record.Attempts <= 1) {
		return ""
	}
	var lines []string
	seen := map[string]bool{}
	for _, hand := range record.Tried {
		sentence := ladderHandSentence(hand)
		if sentence == "" || seen[sentence] {
			continue
		}
		seen[sentence] = true
		lines = append(lines, "- "+sentence)
		if len(lines) >= maxTriedRemedies {
			break
		}
	}
	if record.Attempts > 1 {
		lines = append(lines, fmt.Sprintf("- この工程を %d 回やり直しました。", record.Attempts))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## 本体が試したこと\n" + strings.Join(lines, "\n") + "\n"
}

// ladderHandSentence turns one recorded remedy into words. The names are
// this engine's own fixed strings, so the prefix is enough to know what was
// played; a name from a remedy added later reads as the generic sentence
// rather than leaking an internal word onto the ticket.
func ladderHandSentence(hand string) string {
	name, _, _ := strings.Cut(hand, ":")
	switch name {
	case "seat":
		return "担当の AI を、別のモデルと別の提供元に替えて頼み直しました。"
	case "prompt":
		return "指示の出し方を変えて頼み直しました。"
	case "reclaim":
		return "作業用の保存領域を空けてから、やり直しました。"
	case "route":
		return "通信をやり直しました。"
	case "tool":
		return "必要な道具を入れ直してから、やり直しました。"
	default:
		return "別の手に替えてやり直しました。"
	}
}

// composeAssumptionsText is everything the engine settled without asking:
// what the reception decided instead of putting to the requester, what was
// ruled on when a round stopped agreeing, what was stood in for a key it was
// not given, which roles were moved to another provider, and what now exists
// outside the repository because this delivery made it.
func composeAssumptionsText(runDir, pullRequestURL string, notes *outcomeNotes) string {
	// Where this list sends a reader for the rest depends on whether the
	// description really holds it, so the description is composed first and
	// asked. It costs one more pass over the same records, once per report,
	// and it is the difference between naming a place and naming a page
	// that turns out not to have it either.
	_, whole := composeDecisions(runDir, deliveryListItems, deliveryAssumptionBytes, "", notes)
	text, _ := composeDecisions(runDir, commentListItems, hook.MaxAssumptionsTextBytes,
		outcomeRestPlace(pullRequestURL, whole), notes)
	return text
}

// composeDecisions builds the list for one carrier and says whether that
// carrier held all of it. The two carriers differ only in how much they
// hold and where they send a reader for the rest.
func composeDecisions(runDir string, items, budget int, rest string, notes *outcomeNotes) (string, bool) {
	var builder strings.Builder
	whole := true
	decided, assumed, settledConfidence := receptionAssumptions(runDir, notes)
	decided = append(decided, ruledAssumptions(runDir, notes)...)
	decided = append(decided, returnedAssumptions(runDir, notes)...)

	// The first list, then why it was never put to the requester. A run
	// whose questions the gate settled reaches this comment having asked
	// nobody anything, and the points would otherwise read as points nobody
	// thought worth asking about. The sentence is the plan notice's own, so
	// a requester meeting it twice meets the same words; what the notice
	// adds - how to stop the run - is left off, because by now there is
	// nothing left to stop.
	if !writeOutcomeList(&builder, "## 確認せずに本体が決めたこと", decided, items, rest) {
		whole = false
	}
	if sentence := hook.SettledWithoutAskingSentence(settledConfidence); sentence != "" && len(decided) > 0 {
		builder.WriteString(sentence + "\n")
	}

	for _, list := range []struct {
		heading string
		items   []string
	}{
		{"## 前提とした解釈", assumed},
		{"## 担当の AI を入れ替えたところ", seatMoveLines(runDir, notes)},
		{"## この依頼で作った資源（リポジトリの外にあり、自動では消えません）", createdResourceLines(runDir, notes)},
	} {
		if !writeOutcomeList(&builder, list.heading, list.items, items, rest) {
			whole = false
		}
	}
	text, kept := boundOutcomeText(builder.String(), budget, rest)
	return text, whole && kept
}

// outcomeRestPlace names where the rest of a list can actually be read.
//
// It has to be somewhere that really holds it. The ticket comment used to
// send a reader to "the pull request description and the run history" from
// inside the pull request description, and the run record carries no list of
// decisions at all — so a delivery that decided twenty things told the
// requester that twelve of them were somewhere, and they were nowhere.
//
// The description holds every decision when there is one, because it is
// written after the last round that can make one and its own budget is set
// where no real delivery reaches it. With no pull request there is no
// description, and the honest answer is the run's own records.
func outcomeRestPlace(pullRequestURL string, descriptionHoldsAll bool) string {
	if pullRequestURL != "" && descriptionHoldsAll {
		return "全文は Pull Request の説明にあります: " + pullRequestURL
	}
	return "全文は、運用担当者が保管しているこの実行の記録にあります。"
}

// composeDeliveryPreamble is the same answer, for the pull request the
// engine opens. A reviewer arriving at a diff wants the same two things the
// requester wants: what this is for, and what was decided without anybody
// being asked — the second especially, because it is the part no amount of
// reading the diff will reveal.
//
// Where it can be seen is deliberately absent. Nothing has been merged or
// deployed at the moment this is written, and a description that named a
// screen would be naming one the change has not reached.
func composeDeliveryPreamble(runDir string) string {
	notes := &outcomeNotes{}
	var builder strings.Builder
	if request := outcomeRequest(runDir, notes); request != "" {
		builder.WriteString("## この変更でできるようになること\n")
		builder.WriteString(request + "\n")
	}
	// The whole list, not the comment's share of it. This is the one place
	// every decision can be read, so its own overflow sends a reader to the
	// run's records rather than to the page they are already reading.
	const rest = "全文は、運用担当者が保管しているこの実行の記録にあります。"
	if decided, _ := composeDecisions(runDir, deliveryListItems, deliveryAssumptionBytes, rest, notes); decided != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(decided + "\n")
	}
	if unreadable := notes.line(); unreadable != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(unreadable)
	}
	if builder.Len() == 0 {
		return ""
	}
	preamble, _ := boundOutcomeText(builder.String(), deliveryPreambleBytes, rest)
	return preamble + "\n\n"
}

// deliveryPreambleBytes bounds the whole opening. It leaves the run record
// most of the pull request body, which the record still gives way inside.
const deliveryPreambleBytes = deliveryAssumptionBytes + hook.MaxOutcomeTextBytes

// receptionAssumptions splits what the reception sealed into the points it
// would have asked about and answered itself, and the points it settled from
// the repository or because nobody would ever see them. They are split
// because they are worth different amounts: the first is a decision that was
// the requester's to make, and with nothing asking them anything afterwards
// this comment is the only place they meet it.
func receptionAssumptions(runDir string, notes *outcomeNotes) ([]string, []string, float64) {
	var decided, assumed []string
	var settledConfidence float64
	// The gate's own settled points first, when it settled any. They are
	// the points it had written down to ask about and answered itself
	// instead, so they belong at the head of the list the requester reads
	// for exactly that.
	var sealed struct {
		Fallback            bool            `json:"fallback"`
		InconclusiveReading bool            `json:"inconclusive_reading"`
		Assumptions         []runAssumption `json:"assumptions"`
		ReceptionJudgment   *struct {
			Confidence float64 `json:"confidence"`
		} `json:"reception_judgment"`
	}
	//
	// Read without a note of its own. A decision carries these only when
	// something settled questions, so most runs have none and a run whose
	// decision cannot be read here has the same nothing to show - while the
	// assessment read just below already tells the requester when the
	// reception's record could not be read at all.
	if readOutcomeArtifact(filepath.Join(runDir, "history", "readiness", "decision.json"), &sealed) == nil {
		if sealed.ReceptionJudgment != nil {
			settledConfidence = sealed.ReceptionJudgment.Confidence
		}
		for _, assumption := range sealed.Assumptions {
			if line := assumptionLine(assumption); line != "" && assumption.Kind == assumptionDefensibleDefault {
				decided = append(decided, line)
			}
		}
	}
	// Unusable or inconclusive readings are evidence, not accepted assumptions
	// of the work. In particular, a failed check must not reappear here as a
	// decision made on the requester's behalf.
	for attempt := readinessAssessmentAttempts; !sealed.Fallback && !sealed.InconclusiveReading && attempt >= 1; attempt-- {
		var assessment struct {
			Assumptions []runAssumption `json:"assumptions"`
		}
		path := filepath.Join(runDir, "history", "readiness", fmt.Sprintf("assessment-%d.json", attempt))
		if err := readOutcomeArtifact(path, &assessment); err != nil {
			notes.failed(recordReception, err)
			continue
		}
		for _, assumption := range assessment.Assumptions {
			if line := assumptionLine(assumption); line != "" {
				if assumption.Kind == assumptionDefensibleDefault {
					decided = append(decided, line)
					continue
				}
				assumed = append(assumed, line)
			}
		}
		break
	}
	// Whatever decided something after the reception appends to one stream,
	// because a seat that moves or a round that is ruled on happens while
	// the delivery runs and has no round of its own to be filed under.
	for _, decision := range loadRecordedDecisions(runDir, notes) {
		line := assumptionLine(runAssumption{Statement: decision.Statement, Evidence: decision.Evidence})
		if line == "" {
			continue
		}
		if decision.Decided {
			decided = append(decided, line)
			continue
		}
		assumed = append(assumed, line)
	}
	return decided, assumed, settledConfidence
}

// RecordedDecision is one thing the engine settled while the delivery ran,
// as the places that show them to a requester need it.
//
// Decided separates the ones that were the requester's to make — a point the
// engine took a defensible default on instead of asking, a deadlock it ruled
// on, a key it stood something in for — from the ones nobody would ever have
// been asked about. The requester may want the first kind back, and both the
// plan notice and the closing comment give them a heading of their own.
type RecordedDecision struct {
	Statement string
	Evidence  string
	Decided   bool
}

// LoadRecordedDecisions reads the run's running stream of decisions.
//
// It is exported because the plan notice shows the same decisions the
// closing comment does, and which of them the requester may want back has
// to be one rule in one place. Two copies drift the first time a kind is
// added, and the same decision then has to be shown in one place and may be
// left out of the other.
func LoadRecordedDecisions(runDir string) []RecordedDecision {
	return loadRecordedDecisions(runDir, nil)
}

func loadRecordedDecisions(runDir string, notes *outcomeNotes) []RecordedDecision {
	appended := appendedAssumptions(runDir, notes)
	decisions := make([]RecordedDecision, 0, len(appended))
	for _, assumption := range appended {
		if strings.TrimSpace(assumption.Statement) == "" {
			continue
		}
		decisions = append(decisions, RecordedDecision{
			Statement: assumption.Statement,
			Evidence:  assumption.Evidence,
			Decided:   settledInPlaceOfAsking(assumption.Kind),
		})
	}
	return decisions
}

// settledInPlaceOfAsking reports whether a kind names something the engine
// decided rather than put to the requester.
func settledInPlaceOfAsking(kind string) bool {
	switch kind {
	case assumptionDefensibleDefault, assumptionArbiterRuling, assumptionCredentialStandIn:
		return true
	}
	return false
}

// readinessAssessmentAttempts mirrors the reception's own bound on how many
// times it may assess, newest attempt winning.
const readinessAssessmentAttempts = 3

// appendedAssumptions reads the run's running stream of decisions, one JSON
// object per line. Reception fallback, seat changes and later decisions
// share this stream so plan notices and closing comments read the same facts.
func appendedAssumptions(runDir string, notes *outcomeNotes) []runAssumption {
	encoded, err := readWorkspaceFile(filepath.Join(runDir, "history", "assumptions.jsonl"), maxOutcomeArtifactBytes)
	if err != nil {
		notes.failed(recordDecisions, err)
		return nil
	}
	var appended []runAssumption
	for _, line := range strings.Split(string(encoded), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var assumption runAssumption
		if json.Unmarshal([]byte(line), &assumption) != nil {
			// One bad line loses that line, not the rest. The stream is
			// appended to by several writers over the life of a delivery,
			// and a half-written last line must not hide the others.
			continue
		}
		appended = append(appended, assumption)
	}
	return appended
}

// ruledAssumptions reads what was decided about rounds that stopped
// agreeing. A ruling either sets aside objections the ticket did not ask for
// or tells the implementing role what it has to satisfy; both are decisions
// nobody was asked about, and both belong on the ticket.
//
// The record is read through the arbitrating role's own type and its own
// reader, so a field renamed there stops compiling here rather than quietly
// reporting nothing. A round nobody had to rule on — nearly all of them —
// reads as no ruling; one that is there and will not read is named.
func ruledAssumptions(runDir string, notes *outcomeNotes) []string {
	var lines []string
	for round := 1; round <= maxOutcomeRounds; round++ {
		ruling, err := ReadRuling(runDir, round)
		if err != nil {
			notes.failed(recordRuling, err)
			continue
		}
		if ruling == nil {
			continue
		}
		if line := assumptionLine(runAssumption(ruling.Assumption)); line != "" {
			lines = append(lines, fmt.Sprintf("%d 周目のレビューの行き詰まりについて: %s", round, line))
			continue
		}
		switch ruling.Ruling {
		case worker.RulingOverruleReviewer:
			lines = append(lines, fmt.Sprintf(
				"%d 周目: 依頼が求めている範囲を越えたレビューの指摘 %d 件を退け、変更を通しました。",
				round, len(ruling.Overruled)))
		case worker.RulingInstructImplementer:
			if instruction := clipRunes(ruling.Instruction, outcomeItemRunes); instruction != "" {
				lines = append(lines, fmt.Sprintf("%d 周目: 次の点を満たすよう指示し直しました。%s", round, instruction))
			}
		}
	}
	return lines
}

// returnedAssumptions reads the recorded recovery instructions and what an
// implementing role reported missing. Those quoted reports are not proof
// that a supply is actually required or that a substitute was delivered.
//
// Read structurally, for the same reason as the rulings above.
func returnedAssumptions(runDir string, notes *outcomeNotes) []string {
	var lines []string
	for round := 1; round <= maxOutcomeRounds; round++ {
		var returned struct {
			Returns []struct {
				Supply     []string      `json:"supply"`
				Assumption runAssumption `json:"assumption"`
			} `json:"returns"`
		}
		path := filepath.Join(runDir, "history", "stage-"+strconv.Itoa(round), "returns.json")
		if err := readOutcomeArtifact(path, &returned); err != nil {
			notes.failed(recordReturned, err)
			continue
		}
		for _, entry := range returned.Returns {
			if line := assumptionLine(entry.Assumption); line != "" {
				lines = append(lines, fmt.Sprintf("%d 周目: %s", round, line))
			}
			for _, supply := range entry.Supply {
				if supply = clipRunes(supply, outcomeItemRunes); supply != "" {
					lines = append(lines, "実装役が不足と報告したもの (未検証): "+supply)
				}
			}
		}
	}
	return lines
}

// seatMoveLines reads which roles were run somewhere other than where the
// destination seats them. A night in which two providers went quiet and the
// work was finished by a third is a thing the requester is owed in the
// morning: it says the answer they are reading came from a model they did
// not choose.
func seatMoveLines(runDir string, notes *outcomeNotes) []string {
	var records []SeatRecord
	for _, pattern := range []string{"stage-*", "design-*"} {
		matches, err := filepath.Glob(filepath.Join(runDir, "history", pattern, "*-seat.json"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			var record SeatRecord
			if err := readOutcomeArtifact(path, &record); err != nil {
				notes.failed(recordSeatMove, err)
				continue
			}
			if record.Seat == "" {
				continue
			}
			records = append(records, record)
		}
	}
	// Glob orders lexically, which puts a tenth round before a second one.
	// Two runs of the same request should read the same way, so the order is
	// the one the rounds happened in.
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Round != records[j].Round {
			return records[i].Round < records[j].Round
		}
		return records[i].Seat < records[j].Seat
	})
	var lines []string
	for _, record := range records {
		if record.Candidate <= 0 && record.PromptRebuilt == "" {
			continue
		}
		line := fmt.Sprintf("%d 周目: %s の担当を替えました。", record.Round, record.Seat)
		if record.MovedFrom.Model != "" && record.MovedTo.Model != "" {
			line = fmt.Sprintf("%d 周目: %s の担当を %s から %s に替えました。",
				record.Round, record.Seat, occupantWords(record.MovedFrom), occupantWords(record.MovedTo))
		}
		if reason := clipRunes(record.Reason, outcomeItemRunes); reason != "" {
			line += "理由: " + reason
		}
		lines = append(lines, line)
	}
	return lines
}

func occupantWords(occupant SeatOccupantNote) string {
	if occupant.Vendor == "" {
		return clipRunes(occupant.Model, 60)
	}
	return clipRunes(occupant.Vendor+" の "+occupant.Model, 80)
}

// createdResourceLines reads what now exists outside the repository because
// this delivery made it. A change delivered as a pull request is reviewable
// by reading it; a queue or a bucket the engine brought into existence is
// invisible in the diff, outlives the delivery and costs money until
// somebody knows it is there.
//
// Read structurally: the card that writes this record is not merged here
// yet, and a run without one lists nothing.
func createdResourceLines(runDir string, notes *outcomeNotes) []string {
	encoded, err := readWorkspaceFile(filepath.Join(runDir, "history", "resources.jsonl"), maxOutcomeArtifactBytes)
	if err != nil {
		notes.failed(recordResources, err)
		return nil
	}
	type createdResource struct {
		Kind       string `json:"kind"`
		Identifier string `json:"identifier"`
		Provider   string `json:"provider"`
		// Refused marks a kind the destination did not allow. The card that
		// writes this file records the declaration anyway, because an agent
		// that said it made something may have made it — but it was not
		// created on this delivery's account, and listing it under what the
		// delivery created would tell a requester the engine did a thing it
		// was configured not to do.
		Refused bool `json:"refused"`
	}
	var created []createdResource
	for _, line := range strings.Split(string(encoded), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record createdResource
		if json.Unmarshal([]byte(line), &record) != nil || record.Kind == "" || record.Identifier == "" {
			continue
		}
		if record.Refused {
			continue
		}
		created = append(created, record)
	}
	sort.SliceStable(created, func(i, j int) bool {
		if created[i].Kind != created[j].Kind {
			return created[i].Kind < created[j].Kind
		}
		return created[i].Identifier < created[j].Identifier
	})
	var lines []string
	for _, record := range created {
		line := clipRunes(record.Kind, 64) + ": " + clipRunes(record.Identifier, 256)
		if record.Provider != "" {
			line += "（" + clipRunes(record.Provider, 64) + "）"
		}
		lines = append(lines, line)
	}
	return lines
}

// assumptionLine renders one decision as the sentence a requester reads: what
// was decided, and why that and not something else.
func assumptionLine(assumption runAssumption) string {
	statement := clipRunes(assumption.Statement, outcomeItemRunes)
	if statement == "" {
		return ""
	}
	if evidence := clipRunes(assumption.Evidence, outcomeItemRunes); evidence != "" {
		return statement + "（根拠: " + evidence + "）"
	}
	return statement
}

// writeOutcomeList adds one headed list, counting what it could not fit
// rather than dropping it in silence. An empty list writes nothing at all:
// a heading over nothing tells the reader something is missing.
func writeOutcomeList(builder *strings.Builder, heading string, items []string, limit int, rest string) bool {
	if len(items) == 0 {
		return true
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(heading + "\n")
	for index, item := range items {
		if index >= limit {
			fmt.Fprintf(builder, "- ほか %d 件（%s）\n", len(items)-index, rest)
			return false
		}
		builder.WriteString("- " + item + "\n")
	}
	return true
}

// boundOutcomeText holds a composed section to its budget. The cut lands on
// a character so the text stays valid UTF-8, and says it was cut: a section
// that ends mid-sentence with no note reads as though that was all there was
// to say.
func boundOutcomeText(text string, limit int, rest string) (string, bool) {
	text = strings.TrimRight(text, "\n")
	if text == "" || len(text) <= limit {
		return text, true
	}
	note := "\n…（ここに収まらないため、ここまでを掲示しています。" + rest + "）"
	budget := limit - len(note)
	if budget <= 0 {
		return "", false
	}
	clipped := text[:budget]
	for len(clipped) > 0 && !utf8RuneStart(clipped[len(clipped)-1]) {
		clipped = clipped[:len(clipped)-1]
	}
	// The byte that begins the truncated rune goes too, or the text ends on
	// half a character.
	if len(clipped) > 0 && clipped[len(clipped)-1]&0x80 != 0 {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + note, false
}

// utf8RuneStart reports whether a byte begins a rune (a continuation byte is
// 10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// clipRunes bounds one line and flattens it: these are single lines of a
// list, and a statement carrying its own newlines would break the list it
// sits in.
func clipRunes(value string, limit int) string {
	flattened := strings.Join(strings.Fields(strings.ReplaceAll(value, "\n", " ")), " ")
	runes := []rune(flattened)
	if len(runes) <= limit {
		return flattened
	}
	return string(runes[:limit]) + "…"
}

// readOutcomeArtifact decodes one sealed record. The error is returned as it
// came: a caller tells a record that is simply not there from one that is
// there and will not read by asking the error, and the two are worth very
// different things on the ticket.
func readOutcomeArtifact(path string, out any) error {
	encoded, err := readWorkspaceFile(path, maxOutcomeArtifactBytes)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, out)
}

// latestStageFailure is the last thing that went wrong in this run, whatever
// round or stage it was. The newest record wins because the ladder keeps
// climbing after each one: the earlier records say what the engine already
// tried, and the last says where it was when it stopped.
func latestStageFailure(runDir string, notes *outcomeNotes) (StageFailure, bool) {
	var latest StageFailure
	found := false
	// The delivery cards keep their accounts in a directory of their own —
	// they belong to no round of the implementation — so a run that died
	// merging, waiting for a workflow or looking at a screen has its cause
	// written down here and nowhere else.
	for _, pattern := range []string{"stage-*", "design-*", "deliver-*", "readiness"} {
		matches, err := filepath.Glob(filepath.Join(runDir, "history", pattern, "*-failure.json"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			var failure StageFailure
			if err := readOutcomeArtifact(path, &failure); err != nil {
				notes.failed(recordFailure, err)
				continue
			}
			if failure.Stage == "" || failure.FailedAt.IsZero() {
				continue
			}
			if !found || failure.FailedAt.After(latest.FailedAt) {
				latest, found = failure, true
			}
		}
	}
	return latest, found
}

// ladderAttempts is what the report needs out of the ladder's own record:
// how many times the engine dispatched this step again and which remedies it
// spent. The record is written by the attendant, which cannot be imported
// from here, so the two fields that reach the requester are read by shape.
type ladderAttempts struct {
	Stage    string   `json:"stage"`
	Round    int      `json:"round"`
	Attempts int      `json:"attempts"`
	Tried    []string `json:"tried"`
}

func readLadderAttempts(runDir, stage string, round int, notes *outcomeNotes) (ladderAttempts, bool) {
	if stage == "" || round < 1 {
		return ladderAttempts{}, false
	}
	var record ladderAttempts
	path := filepath.Join(runDir, "retry", fmt.Sprintf("%s-r%d.json", stage, round))
	if err := readOutcomeArtifact(path, &record); err != nil {
		notes.failed(recordLadder, err)
		return ladderAttempts{}, false
	}
	if record.Stage != stage || record.Round != round {
		return ladderAttempts{}, false
	}
	return record, true
}

// outcomeUnapplied names the parts of the destination's release path this
// delivery did not apply.
//
// By their configuration keys and nothing else. These are the parts the
// engine was not handed the means for — a credential it does not hold, a
// permission the destination's own policy refuses, its own execution
// settings — and the previous shape of this was a line on the ticket
// telling a person to go and write them. That line is what the release
// path work exists to remove, so what is left is a statement of fact in the
// report: here is what this delivery did not touch. Nobody is asked for
// anything.
func outcomeUnapplied(runDir string, notes *outcomeNotes) string {
	plan, err := worker.ReadReleasePathFile(ReleasePathPlanFile(runDir))
	if err != nil {
		// A plan that is not there is the ordinary case: most destinations
		// have a complete release path or stop at the proposal. One that is
		// there and will not read is named with the other unreadable
		// records rather than passed over, because an absent section reads
		// as "nothing was left undone".
		if _, statErr := os.Stat(ReleasePathPlanFile(runDir)); statErr == nil {
			notes.failed(recordPath, err)
		}
		return ""
	}
	names := plan.UnappliedNames()
	if len(names) == 0 {
		return ""
	}
	return "## この実行で本体が適用していない設定\n" +
		"次の設定は、本体に渡されている手段では書けないため、この実行では触っていません: " +
		strings.Join(names, "、") + "\n"
}
