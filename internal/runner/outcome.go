package runner

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
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

	// outcomeItemRunes bounds one line of a list, and outcomeListItems how
	// many lines one list may have. What is over the count is counted rather
	// than dropped in silence, so a delivery that decided thirty things
	// still says it decided thirty.
	outcomeItemRunes = 200
	outcomeListItems = 8

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

// composeOutcome writes the two sections the report leads with, out of the
// run directory. Nothing here can fail the report: a record that is missing,
// unreadable or the wrong shape leaves its part out, because a delivery that
// finished must not be unable to say so because one file went bad.
func composeOutcome(runDir string, code hook.TerminalCode, evidence map[string]string) (string, string) {
	return composeOutcomeText(runDir, code, evidence), composeAssumptionsText(runDir)
}

// composeOutcomeText says what the delivery made possible and where that can
// be seen — or, for a delivery that ended in a failure, what happened and
// what the engine tried about it.
func composeOutcomeText(runDir string, code hook.TerminalCode, evidence map[string]string) string {
	var builder strings.Builder
	if request := outcomeRequest(runDir); request != "" {
		builder.WriteString("## この依頼でできるようになったこと\n")
		builder.WriteString(request + "\n")
	}
	if where := outcomeWhereToSee(runDir, code, evidence); where != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(where)
	}
	if happened := outcomeWhatHappened(runDir, code, evidence); happened != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(happened)
	}
	return boundOutcomeText(builder.String(), hook.MaxOutcomeTextBytes)
}

// outcomeRequest is the engine's own reading of what was asked for, written
// back as the thing that is now done. It comes from the reception's sealed
// ticket, and from the draft the reception was built from when the run never
// got as far as sealing one.
func outcomeRequest(runDir string) string {
	var ticket struct {
		Request string `json:"request"`
	}
	if readOutcomeArtifact(filepath.Join(runDir, "readiness-ticket.json"), &ticket) == nil {
		if request := clipRunes(ticket.Request, outcomeProseRunes); request != "" {
			return request
		}
	}
	var draft struct {
		Request string `json:"request"`
	}
	if readOutcomeArtifact(filepath.Join(runDir, "ticket-draft.json"), &draft) == nil {
		return clipRunes(draft.Request, outcomeProseRunes)
	}
	return ""
}

// outcomeWhereToSee names the places the change can be looked at, and what
// the engine saw when it looked. The depth the run actually reached decides
// which places those are: a delivery that stopped at its proposal has no
// screen to point at, and says so rather than pointing at one it never
// touched.
func outcomeWhereToSee(runDir string, code hook.TerminalCode, evidence map[string]string) string {
	if code != hook.TerminalSuccess {
		return ""
	}
	var lines []string
	if url := evidence["production_evidence_url"]; url != "" {
		lines = append(lines, "production確認先: "+url)
		if seen := observedLine(runDir, DeliverProductionReportFile, "本番"); seen != "" {
			lines = append(lines, seen)
		}
	}
	if url := evidence["staging_evidence_url"]; url != "" {
		lines = append(lines, "staging確認先: "+url)
		if seen := observedLine(runDir, DeliverStagingReportFile, "staging"); seen != "" {
			lines = append(lines, seen)
		}
	}
	if len(lines) == 0 {
		// The merge-and-look card of the older delivery path leaves its own
		// verdict, and a delivery that only proposed a change has neither.
		if seen := e2eObservedLine(runDir); seen != "" {
			lines = append(lines, seen)
		} else {
			lines = append(lines, "まだ動いている場所はありません。提案した変更は、下の Pull Request でご確認ください。")
		}
	}
	return "## どこで見られるか\n" + strings.Join(lines, "\n") + "\n"
}

// observedLine says, in one sentence, what the observation of one phase
// actually saw. A phase that passed without looking at a screen says that
// too: a pass the engine never verified with its eyes must not read as one
// it did.
func observedLine(runDir, file, place string) string {
	report, found := ReadDeliverReport(runDir, file)
	if !found {
		return ""
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
func e2eObservedLine(runDir string) string {
	result, found := ReadE2EResult(runDir)
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
func outcomeWhatHappened(runDir string, code hook.TerminalCode, evidence map[string]string) string {
	if code == hook.TerminalSuccess || code == hook.TerminalInvestigated || code == hook.TerminalCancelled {
		return ""
	}
	failure, found := latestStageFailure(runDir)
	step := evidence["failed_step"]
	if !found && step == "" {
		return ""
	}
	var lines []string
	switch {
	case step != "" && found && failure.Round > 0:
		lines = append(lines, fmt.Sprintf("%s の工程が、%d 周目で完了しませんでした。", step, failure.Round))
	case step != "":
		lines = append(lines, step+" の工程が完了しませんでした。")
	default:
		lines = append(lines, "工程のひとつが完了しませんでした。")
	}
	if found {
		if failure.Interrupted {
			lines = append(lines, "原因: 処理を動かしている場所が入れ替わり、工程が途中で止まりました。")
		} else if reason := failureClassSentence(failure.Class); reason != "" {
			lines = append(lines, "原因: "+reason)
		}
	}
	text := "## 何が起きたか\n" + strings.Join(lines, "\n") + "\n"
	if tried := outcomeWhatWasTried(runDir, failure.Stage, failure.Round); tried != "" {
		text += "\n" + tried
	}
	return text
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
	default:
		return ""
	}
}

// outcomeWhatWasTried lists the remedies the engine spent on the step that
// stopped. It is the half of the account that says the engine did not just
// sit there, and the half a person needs to decide whether to change
// anything before asking again.
func outcomeWhatWasTried(runDir, stage string, round int) string {
	record, found := readLadderAttempts(runDir, stage, round)
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
		if len(lines) >= outcomeListItems {
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
func composeAssumptionsText(runDir string) string {
	var builder strings.Builder
	decided, assumed := receptionAssumptions(runDir)
	decided = append(decided, ruledAssumptions(runDir)...)
	decided = append(decided, returnedAssumptions(runDir)...)

	writeOutcomeList(&builder, "## 確認せずに本体が決めたこと", decided)
	writeOutcomeList(&builder, "## 前提とした解釈", assumed)
	writeOutcomeList(&builder, "## 担当の AI を入れ替えたところ", seatMoveLines(runDir))
	writeOutcomeList(&builder, "## この依頼で作った資源（リポジトリの外にあり、自動では消えません）", createdResourceLines(runDir))
	return boundOutcomeText(builder.String(), hook.MaxAssumptionsTextBytes)
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
	var builder strings.Builder
	if request := outcomeRequest(runDir); request != "" {
		builder.WriteString("## この変更でできるようになること\n")
		builder.WriteString(request + "\n")
	}
	if decided := composeAssumptionsText(runDir); decided != "" {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(decided + "\n")
	}
	if builder.Len() == 0 {
		return ""
	}
	return boundOutcomeText(builder.String(), hook.MaxOutcomeTextBytes+hook.MaxAssumptionsTextBytes) + "\n\n"
}

// receptionAssumptions splits what the reception sealed into the points it
// would have asked about and answered itself, and the points it settled from
// the repository or because nobody would ever see them. They are split
// because they are worth different amounts: the first is a decision that was
// the requester's to make, and with nothing asking them anything afterwards
// this comment is the only place they meet it.
func receptionAssumptions(runDir string) ([]string, []string) {
	var decided, assumed []string
	for attempt := readinessAssessmentAttempts; attempt >= 1; attempt-- {
		var assessment struct {
			Assumptions []runAssumption `json:"assumptions"`
		}
		path := filepath.Join(runDir, "history", "readiness", fmt.Sprintf("assessment-%d.json", attempt))
		if readOutcomeArtifact(path, &assessment) != nil {
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
	for _, decision := range LoadRecordedDecisions(runDir) {
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
	return decided, assumed
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
	appended := appendedAssumptions(runDir)
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
// object per line. Nothing writes it on this engine yet; the reader is here
// because the decisions it will carry are made while the delivery runs, and
// a report composed from per-round records alone would have to be changed
// again for each new producer of one.
func appendedAssumptions(runDir string) []runAssumption {
	encoded, err := readWorkspaceFile(filepath.Join(runDir, "history", "assumptions.jsonl"), maxOutcomeArtifactBytes)
	if err != nil {
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
// The record's shape is the one the arbitrating role seals. It is read
// structurally rather than through that package's type because the package
// is not merged here yet: a run without rulings finds no files and lists
// nothing, and a run with them lists them the day they appear.
func ruledAssumptions(runDir string) []string {
	var lines []string
	for round := 1; round <= maxOutcomeRounds; round++ {
		var ruling struct {
			Ruling      string            `json:"ruling"`
			Instruction string            `json:"instruction"`
			Overruled   []json.RawMessage `json:"overruled"`
			Assumption  runAssumption     `json:"assumption"`
		}
		path := filepath.Join(runDir, "history", "stage-"+strconv.Itoa(round), "ruling.json")
		if readOutcomeArtifact(path, &ruling) != nil {
			continue
		}
		if line := assumptionLine(ruling.Assumption); line != "" {
			lines = append(lines, fmt.Sprintf("%d 周目のレビューの行き詰まりについて: %s", round, line))
			continue
		}
		switch ruling.Ruling {
		case "overrule_reviewer":
			lines = append(lines, fmt.Sprintf(
				"%d 周目: 依頼が求めている範囲を越えたレビューの指摘 %d 件を退け、変更を通しました。",
				round, len(ruling.Overruled)))
		case "instruct_implementer":
			if instruction := clipRunes(ruling.Instruction, outcomeItemRunes); instruction != "" {
				lines = append(lines, fmt.Sprintf("%d 周目: 次の点を満たすよう指示し直しました。%s", round, instruction))
			}
		}
	}
	return lines
}

// returnedAssumptions reads what the engine put in place of what an
// implementing role said it was missing. A role that hands the work back
// asking for a decision is answered with the reading easiest to defend, and
// one asking for a key is answered with a stand-in and a note of what has to
// be supplied for the real thing — and the note is the part the requester
// cannot be left without.
//
// Read structurally, for the same reason as the rulings above.
func returnedAssumptions(runDir string) []string {
	var lines []string
	for round := 1; round <= maxOutcomeRounds; round++ {
		var returned struct {
			Returns []struct {
				Supply     []string      `json:"supply"`
				Assumption runAssumption `json:"assumption"`
			} `json:"returns"`
		}
		path := filepath.Join(runDir, "history", "stage-"+strconv.Itoa(round), "returns.json")
		if readOutcomeArtifact(path, &returned) != nil {
			continue
		}
		for _, entry := range returned.Returns {
			if line := assumptionLine(entry.Assumption); line != "" {
				lines = append(lines, fmt.Sprintf("%d 周目: %s", round, line))
			}
			for _, supply := range entry.Supply {
				if supply = clipRunes(supply, outcomeItemRunes); supply != "" {
					lines = append(lines, "本物として供給が必要なもの: "+supply)
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
func seatMoveLines(runDir string) []string {
	var records []SeatRecord
	for _, pattern := range []string{"stage-*", "design-*"} {
		matches, err := filepath.Glob(filepath.Join(runDir, "history", pattern, "*-seat.json"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			var record SeatRecord
			if readOutcomeArtifact(path, &record) != nil || record.Seat == "" {
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
func createdResourceLines(runDir string) []string {
	encoded, err := readWorkspaceFile(filepath.Join(runDir, "history", "resources.jsonl"), maxOutcomeArtifactBytes)
	if err != nil {
		return nil
	}
	type createdResource struct {
		Kind       string `json:"kind"`
		Identifier string `json:"identifier"`
		Provider   string `json:"provider"`
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
func writeOutcomeList(builder *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(heading + "\n")
	for index, item := range items {
		if index >= outcomeListItems {
			fmt.Fprintf(builder, "- ほか %d 件（全文は Pull Request の説明と実行履歴にあります）\n", len(items)-index)
			break
		}
		builder.WriteString("- " + item + "\n")
	}
}

// boundOutcomeText holds a composed section to its budget. The cut lands on
// a character so the text stays valid UTF-8, and says it was cut: a section
// that ends mid-sentence with no note reads as though that was all there was
// to say.
func boundOutcomeText(text string, limit int) string {
	text = strings.TrimRight(text, "\n")
	if text == "" || len(text) <= limit {
		return text
	}
	const note = "\n…（このコメントに収まらないため、ここまでを掲示しています。全文は Pull Request の説明と実行履歴にあります）"
	budget := limit - len(note)
	if budget <= 0 {
		return ""
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
	return clipped + note
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
func latestStageFailure(runDir string) (StageFailure, bool) {
	var latest StageFailure
	found := false
	for _, pattern := range []string{"stage-*", "design-*", "readiness"} {
		matches, err := filepath.Glob(filepath.Join(runDir, "history", pattern, "*-failure.json"))
		if err != nil {
			continue
		}
		for _, path := range matches {
			var failure StageFailure
			if readOutcomeArtifact(path, &failure) != nil || failure.Stage == "" || failure.FailedAt.IsZero() {
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

func readLadderAttempts(runDir, stage string, round int) (ladderAttempts, bool) {
	if stage == "" || round < 1 {
		return ladderAttempts{}, false
	}
	var record ladderAttempts
	path := filepath.Join(runDir, "retry", fmt.Sprintf("%s-r%d.json", stage, round))
	if readOutcomeArtifact(path, &record) != nil || record.Stage != stage || record.Round != round {
		return ladderAttempts{}, false
	}
	return record, true
}
