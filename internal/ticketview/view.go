// Package ticketview assembles what one delivery's run records say into the
// view a person reads on the board's ticket page: the timeline of steps
// with each step's outcome and reason, the evidence behind each, what the
// run cost, and why it stopped if it stopped. It reads the run directory
// only - the same files the status board classifies from - and writes
// nothing. Every free text that came from a model or a repository passes
// the store-time secret scan again before it is shown.
package ticketview

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker"
)

// View is the ticket page's data.
type View struct {
	DeliveryID     string   `json:"delivery_id"`
	IssueKey       string   `json:"issue_key,omitempty"`
	Summary        string   `json:"summary,omitempty"`
	Repository     string   `json:"repository,omitempty"`
	Request        string   `json:"request,omitempty"`
	PullRequestURL string   `json:"pr_url,omitempty"`
	MergeSHA       string   `json:"merge_sha,omitempty"`
	Timeline       []Event  `json:"timeline"`
	Cost           Cost     `json:"cost"`
	Failure        *Failure `json:"failure,omitempty"`
	// Running is the step the runner started and has not finished. It is
	// what tells a reader that a run is working rather than stuck: before
	// it existed, a reception of a dozen steps showed one unchanging line
	// for minutes, and an in-progress round read as a failed one.
	Running *RunningStep `json:"running,omitempty"`
	Records []string     `json:"records"`
	BuiltAt time.Time    `json:"built_at"`
}

// RunningStep is the step the runner is executing right now.
type RunningStep struct {
	Step      string    `json:"step"`
	StartedAt time.Time `json:"started_at"`
}

// Event is one step on the timeline. Tone is ok | warn | bad | neutral.
type Event struct {
	At       time.Time  `json:"at"`
	Step     string     `json:"step"`
	Title    string     `json:"title"`
	Tone     string     `json:"tone"`
	Why      string     `json:"why,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
	Record   string     `json:"record,omitempty"`
}

// Evidence is one labelled piece of what a step recorded.
type Evidence struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Cost is what the records say the run cost, summed from the model
// invocations that recorded a price. Note says what the sum covers.
type Cost struct {
	Lines    []CostLine `json:"lines"`
	TotalUSD float64    `json:"total_usd"`
	Note     string     `json:"note,omitempty"`
	// Billed is what the gateway billed the run's keys, read at the end of
	// the run and kept in spend.json - the same figures the terminal
	// comment carries, for a failed run too.
	Billed *Billed `json:"billed,omitempty"`
}

// Billed is the gateway's own reading of what the run cost.
type Billed struct {
	ReadAt   time.Time    `json:"read_at"`
	Complete bool         `json:"complete"`
	TotalUSD float64      `json:"total_usd"`
	Lines    []BilledLine `json:"lines"`
}

// BilledLine is one key's figure with the roles it serves.
type BilledLine struct {
	Label    string  `json:"label"`
	KeyName  string  `json:"key_name,omitempty"`
	USD      float64 `json:"usd"`
	Unpriced int     `json:"unpriced,omitempty"`
}

type CostLine struct {
	Label string  `json:"label"`
	USD   float64 `json:"usd"`
}

// Failure is why the run stopped, when it stopped for a failure.
type Failure struct {
	Step   string `json:"step"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// Model is what the worker knew when a reception model turn gave up
	// (model-failure-detail.json): the phrase, the calls, the last answer.
	Model *ModelFailure `json:"model,omitempty"`
}

// ModelFailure is the recorded detail of a failed model turn.
type ModelFailure struct {
	RecordedAt           time.Time `json:"recorded_at"`
	Phrase               string    `json:"phrase"`
	Model                string    `json:"model,omitempty"`
	Effort               string    `json:"effort,omitempty"`
	MaxOutputTokens      int32     `json:"max_output_tokens,omitempty"`
	FinalEffort          string    `json:"final_effort,omitempty"`
	FinalMaxOutputTokens int32     `json:"final_max_output_tokens,omitempty"`
	Calls                int       `json:"calls"`
	Lowered              int       `json:"lowered,omitempty"`
	Widened              bool      `json:"widened,omitempty"`
	Malformed            int       `json:"malformed,omitempty"`
	ProviderErrors       int       `json:"provider_errors,omitempty"`
	AllowanceSpent       int       `json:"allowance_spent,omitempty"`
	LastRequestID        string    `json:"last_request_id,omitempty"`
	LastFinishReason     string    `json:"last_finish_reason,omitempty"`
	LastPromptTokens     int32     `json:"last_prompt_tokens,omitempty"`
	LastCompletionTokens int32     `json:"last_completion_tokens,omitempty"`
	LastReasoningTokens  int32     `json:"last_reasoning_tokens,omitempty"`
	LastHTTPStatus       int       `json:"last_http_status,omitempty"`
	// Objection is what the contract said about the last answer when the
	// AI answered but the answer could not be used.
	Objection string `json:"objection,omitempty"`
	// Summary says in the requester's words what the numbers mean.
	Summary string `json:"summary,omitempty"`
}

// RecordNames are the run-directory files the raw record endpoint may
// serve, by the name the page links; anything else is refused.
var RecordNames = map[string]string{
	"intake":                    "intake.json",
	"readiness-decision":        "history/readiness/decision.json",
	"validation":                "validation.json",
	"feature-pr":                "feature-pr.json",
	"feature-checks":            "feature-checks.json",
	"feature-merge":             "feature-merge.json",
	"deliver-staging-report":    "deliver-staging-report.json",
	"deliver-production-report": "deliver-production-report.json",
	"board-outcome":             "board-outcome.json",
	"model-failure":             "model-failure.json",
	"model-failure-detail":      "model-failure-detail.json",
	"spend":                     "spend.json",
	"failed-step":               "failed-step.txt",
	"trail":                     "m1-trail.txt",
	"measurements":              "measurements.jsonl",
}

// The reception makes up to three assess/check attempts; a stage or a
// design round is a directory of JSON records named by the engine and its
// configured reviewer ids (letters, digits, hyphens - never a path).
var (
	readinessRecord = regexp.MustCompile(`^readiness-(assessment|check)-([1-3])$`)
	stageRecord     = regexp.MustCompile(`^stage-([1-9][0-9]?)-([a-z][a-z0-9-]{0,80})$`)
	designRecord    = regexp.MustCompile(`^design-([1-9][0-9]?)-([a-z][a-z0-9-]{0,80})$`)
	recordBase      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,80}$`)
)

// RecordPath resolves a record name the page may link to the file under the
// run directory, or "" for a name it does not serve. Readiness attempts are
// "readiness-assessment-<n>" / "readiness-check-<n>"; stage records are
// "stage-<n>-<file>" (stage-1-review-a, stage-2-candidate); design-round
// records are "design-<n>-<file>" (design-1-investigation).
func RecordPath(name string) string {
	if path, ok := RecordNames[name]; ok {
		return path
	}
	if m := readinessRecord.FindStringSubmatch(name); m != nil {
		return filepath.Join("history", "readiness", m[1]+"-"+m[2]+".json")
	}
	if m := stageRecord.FindStringSubmatch(name); m != nil {
		return filepath.Join("history", "stage-"+m[1], m[2]+".json")
	}
	if m := designRecord.FindStringSubmatch(name); m != nil {
		return filepath.Join("history", "design-"+m[1], m[2]+".json")
	}
	return ""
}

// maxText bounds any one piece of evidence shown on the page.
const maxText = 4000

// shown passes a free text through the secret scan and bounds it. A text
// the scan refuses whole (a private key, a known value) is replaced by a
// note; masked values keep their markers.
func shown(text string) string {
	if text == "" {
		return ""
	}
	masked, _, refusal := probe.MaskSecrets(text, nil)
	if refusal != "" {
		return "[秘密の形 (" + refusal + ") を含むため、この記録は表示しません]"
	}
	if len(masked) > maxText {
		cut := maxText
		for cut > 0 && cut < len(masked) && (masked[cut]&0xC0) == 0x80 {
			cut--
		}
		masked = masked[:cut] + " …(以下略)"
	}
	return masked
}

// shownTail is the end of a long text (a transcript, the trail) after the
// secret scan: the whole text is masked first, so a value straddling the
// cut cannot lose its prefix and pass, and the tail is taken from the
// masked whole - never from shown's head-first bound.
func shownTail(text string, n int) string {
	if text == "" {
		return ""
	}
	masked, _, refusal := probe.MaskSecrets(text, nil)
	if refusal != "" {
		return "[秘密の形 (" + refusal + ") を含むため、この記録は表示しません]"
	}
	return tail(masked, n)
}

// Build reads the run directory and assembles the view. A missing run
// directory is an error; any missing record inside it is simply absent from
// the view, never an error - the page shows what the run recorded so far.
func Build(runDir string) (View, error) {
	if info, err := os.Stat(runDir); err != nil || !info.IsDir() {
		return View{}, errors.New("run directory not found")
	}
	view := View{DeliveryID: filepath.Base(runDir), BuiltAt: time.Now().UTC(), Timeline: []Event{}, Records: []string{}}
	view.readRunning(runDir)
	view.readTicket(runDir)
	view.readIntake(runDir)
	view.readReadiness(runDir)
	view.readStages(runDir)
	view.readDesignRounds(runDir)
	view.readValidation(runDir)
	view.readDelivery(runDir)
	view.readEnding(runDir)
	view.readSpend(runDir)
	view.readMeasurements(runDir)
	sort.SliceStable(view.Timeline, func(i, j int) bool { return view.Timeline[i].At.Before(view.Timeline[j].At) })
	for name := range RecordNames {
		if exists(filepath.Join(runDir, RecordNames[name])) {
			view.Records = append(view.Records, name)
		}
	}
	for n := 1; n <= 3; n++ {
		for _, kind := range []string{"assessment", "check"} {
			if exists(filepath.Join(runDir, "history", "readiness", fmt.Sprintf("%s-%d.json", kind, n))) {
				view.Records = append(view.Records, fmt.Sprintf("readiness-%s-%d", kind, n))
			}
		}
	}
	// Every JSON record of every stage and design round, by the name the
	// page serves it under.
	for _, kind := range []string{"stage", "design"} {
		for n := 1; n <= 20; n++ {
			dir := filepath.Join(runDir, "history", fmt.Sprintf("%s-%d", kind, n))
			entries, err := os.ReadDir(dir)
			if err != nil {
				break
			}
			for _, entry := range entries {
				base, isJSON := strings.CutSuffix(entry.Name(), ".json")
				if entry.Type().IsRegular() && isJSON && recordBase.MatchString(base) {
					view.Records = append(view.Records, fmt.Sprintf("%s-%d-%s", kind, n, base))
				}
			}
		}
	}
	sort.Strings(view.Records)
	view.Cost.Note = "記録に価格が残っている呼び出しの合計 (コードのレビュー役の呼び出しは価格を残さないので含まれない。設計レビューは含む)"
	return view, nil
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readJSON(path string, into any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, into) == nil
}

func fileTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime().UTC()
}

type invocation struct {
	RequestedModel string  `json:"requested_model"`
	StopReason     string  `json:"stop_reason"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CostUSD        float64 `json:"cost_usd"`
}

func (v *View) addCost(label string, usd float64) {
	if usd <= 0 {
		return
	}
	v.Cost.Lines = append(v.Cost.Lines, CostLine{Label: label, USD: usd})
	v.Cost.TotalUSD += usd
}

func (v *View) readTicket(runDir string) {
	var ticket struct {
		IssueKey   string `json:"issue_key"`
		Summary    string `json:"summary"`
		Repository string `json:"repository"`
		Request    string `json:"request"`
	}
	for _, name := range []string{filepath.Join("history", "stage-1", "ticket.json"), "readiness-ticket.json"} {
		if readJSON(filepath.Join(runDir, name), &ticket) && ticket.IssueKey != "" {
			break
		}
	}
	v.IssueKey, v.Summary, v.Repository, v.Request = ticket.IssueKey, shown(ticket.Summary), ticket.Repository, shown(ticket.Request)
}

func (v *View) readIntake(runDir string) {
	var intake struct {
		Model      string     `json:"model"`
		ReadAt     time.Time  `json:"read_at"`
		Rationale  string     `json:"rationale"`
		Invocation invocation `json:"invocation"`
	}
	path := filepath.Join(runDir, "intake.json")
	if !readJSON(path, &intake) {
		return
	}
	v.addCost("受付 (読み取り)", intake.Invocation.CostUSD)
	v.Timeline = append(v.Timeline, Event{
		At: intake.ReadAt, Step: "intake", Title: "受付: 依頼を読み取った", Tone: "ok",
		Why:      shown(intake.Rationale),
		Evidence: []Evidence{{Label: "受付役のモデル", Text: intake.Model}},
		Record:   "intake",
	})
}

// readReadiness reads the reception's judgment: up to three assess/check
// attempts (a check that fails sends the assessor back once more), then
// the sealed decision. Earlier attempts show as sent-back rounds with the
// checker's reasons; the last attempt carries the assumptions and questions
// the decision rests on.
func (v *View) readReadiness(runDir string) {
	dir := filepath.Join(runDir, "history", "readiness")
	type assessment struct {
		Decision  string `json:"decision"`
		Questions []struct {
			Text string `json:"text"`
		} `json:"questions"`
		Assumptions []struct {
			Statement string `json:"statement"`
		} `json:"assumptions"`
		RequestKind     string     `json:"request_kind"`
		ApproachExcerpt string     `json:"approach_excerpt"`
		NeedsDesign     bool       `json:"needs_design"`
		DesignReason    string     `json:"design_reason"`
		Model           string     `json:"model"`
		Invocation      invocation `json:"invocation"`
	}
	type check struct {
		Verdict    string     `json:"verdict"`
		Reasons    []string   `json:"reasons"`
		Model      string     `json:"model"`
		CheckedAt  time.Time  `json:"checked_at"`
		Invocation invocation `json:"invocation"`
	}
	var decision struct {
		Outcome      string `json:"outcome"`
		NeedsDesign  bool   `json:"needs_design"`
		DesignReason string `json:"design_reason"`
		RequestKind  string `json:"request_kind"`
	}
	haveDecision := readJSON(filepath.Join(dir, "decision.json"), &decision)
	var last assessment
	var lastCheck check
	var lastAt time.Time
	haveAssessment, haveCheck, lastN := false, false, 0
	for n := 1; n <= 3; n++ {
		var a assessment
		var c check
		assessmentPath := filepath.Join(dir, fmt.Sprintf("assessment-%d.json", n))
		haveA := readJSON(assessmentPath, &a)
		haveC := readJSON(filepath.Join(dir, fmt.Sprintf("check-%d.json", n)), &c)
		if !haveA && !haveC {
			break
		}
		suffix := ""
		if n > 1 || exists(filepath.Join(dir, "assessment-2.json")) {
			suffix = fmt.Sprintf(" %d 回目", n)
		}
		v.addCost("受付の判定"+suffix+" (起案役)", a.Invocation.CostUSD)
		v.addCost("受付の判定"+suffix+" (確認役)", c.Invocation.CostUSD)
		at := c.CheckedAt
		if at.IsZero() {
			at = fileTime(assessmentPath)
		}
		if exists(filepath.Join(dir, fmt.Sprintf("assessment-%d.json", n+1))) {
			// Sent back: the checker refused this attempt and the assessor
			// tried again. Its reasons are the whole story of this round.
			event := Event{At: at, Step: "readiness", Tone: "warn", Title: fmt.Sprintf("受付の判定 %d 回目: 確認役が差し戻し", n), Record: fmt.Sprintf("readiness-check-%d", n)}
			if len(c.Reasons) > 0 {
				event.Why = shown(strings.Join(c.Reasons, " / "))
			}
			v.Timeline = append(v.Timeline, event)
			continue
		}
		last, lastCheck, haveAssessment, haveCheck, lastAt, lastN = a, c, haveA, haveC, at, n
	}
	if !haveAssessment && !haveCheck && !haveDecision {
		return
	}
	at := lastAt
	if at.IsZero() {
		at = fileTime(filepath.Join(dir, "decision.json"))
	}
	event := Event{At: at, Step: "readiness", Tone: "ok", Record: "readiness-decision"}
	if !haveDecision && lastN > 0 {
		event.Record = fmt.Sprintf("readiness-assessment-%d", lastN)
	}
	outcome := decision.Outcome
	if outcome == "" {
		outcome = last.Decision
	}
	switch outcome {
	case "ready":
		event.Title = "受付の判定: 実装へ"
	case "clarification", "needs_clarification":
		event.Title, event.Tone = "受付の判定: 依頼者に質問", "warn"
	case "":
		event.Title, event.Tone = "受付の判定: 記録が途中", "warn"
	default:
		event.Title, event.Tone = "受付の判定: "+outcome, "warn"
	}
	if lastN > 1 {
		event.Title += fmt.Sprintf(" (%d 回目で確定)", lastN)
	}
	needsDesign, reason := decision.NeedsDesign, decision.DesignReason
	if !haveDecision {
		needsDesign, reason = last.NeedsDesign, last.DesignReason
	}
	if reason != "" {
		// The same sentence the plan notice carries; an unknown code still
		// reads as a decision, not as a code.
		event.Why = hook.DesignDecisionLine(needsDesign, reason)
	}
	if last.ApproachExcerpt != "" {
		event.Evidence = append(event.Evidence, Evidence{Label: "本文から引用した方針", Text: shown(last.ApproachExcerpt)})
	}
	for i, a := range last.Assumptions {
		event.Evidence = append(event.Evidence, Evidence{Label: fmt.Sprintf("前提 %d", i+1), Text: shown(a.Statement)})
	}
	for i, q := range last.Questions {
		event.Evidence = append(event.Evidence, Evidence{Label: fmt.Sprintf("質問 %d", i+1), Text: shown(q.Text)})
	}
	if haveCheck {
		text := "判定: " + lastCheck.Verdict
		if len(lastCheck.Reasons) > 0 {
			text += " — " + shown(strings.Join(lastCheck.Reasons, " / "))
		}
		event.Evidence = append(event.Evidence, Evidence{Label: "確認役 (" + lastCheck.Model + ")", Text: text})
	}
	if last.Model != "" {
		event.Evidence = append(event.Evidence, Evidence{Label: "起案役", Text: last.Model})
	}
	v.Timeline = append(v.Timeline, event)
}

func (v *View) readStages(runDir string) {
	for n := 1; n <= 20; n++ {
		dir := filepath.Join(runDir, "history", fmt.Sprintf("stage-%d", n))
		if !exists(dir) {
			break
		}
		v.readImplementation(dir, n)
		v.readReviews(dir, n)
	}
}

func (v *View) readImplementation(dir string, n int) {
	var candidate struct {
		GeneratedAt time.Time  `json:"generated_at"`
		Rationale   string     `json:"rationale"`
		Invocation  invocation `json:"invocation"`
		Files       []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	var run struct {
		RanAt        time.Time `json:"ran_at"`
		DurationMS   int64     `json:"duration_ms"`
		ExitCode     int       `json:"exit_code"`
		ChangedFiles []string  `json:"changed_files"`
		Transcript   string    `json:"transcript"`
		AgentID      string    `json:"agent_id"`
	}
	haveCandidate := readJSON(filepath.Join(dir, "candidate.json"), &candidate)
	// The implementer's own report (implementer-run), the applier's in a
	// design-backed round (applier-run), or the seal's copy (implement-run).
	runRecord := ""
	var haveRun bool
	for _, base := range []string{"implementer-run", "applier-run", "implement-run"} {
		if haveRun = readJSON(filepath.Join(dir, base+".json"), &run); haveRun {
			runRecord = base
			break
		}
	}
	if !haveCandidate && !haveRun {
		return
	}
	v.addCost(fmt.Sprintf("実装 %d 巡目", n), candidate.Invocation.CostUSD)
	at := candidate.GeneratedAt
	if at.IsZero() {
		at = run.RanAt
	}
	files := run.ChangedFiles
	if len(files) == 0 {
		for _, f := range candidate.Files {
			files = append(files, f.Path)
		}
	}
	event := Event{At: at, Step: "implement", Tone: "ok", Title: fmt.Sprintf("実装 %d 巡目: 変更 %d ファイル", n, len(files)), Record: fmt.Sprintf("stage-%d-candidate", n)}
	if !haveCandidate {
		// The implementer's report lands before the change is sealed, and
		// between the two the round looked failed in red while it was in
		// fact running (live 2026-09-17: a reader almost told the requester
		// the round had failed). A run that is still executing steps is
		// told as in progress; only a run that has stopped can be said to
		// have sealed nothing.
		event.Title, event.Tone = fmt.Sprintf("実装 %d 巡目: 変更が封緘されなかった", n), "bad"
		if v.Running != nil {
			event.Title, event.Tone = fmt.Sprintf("実装 %d 巡目: 変更を封緘中", n), "neutral"
		}
		event.Record = fmt.Sprintf("stage-%d-%s", n, runRecord)
	}
	if runRecord == "applier-run" {
		event.Title = strings.Replace(event.Title, "実装", "設計に沿った実装", 1)
		if !haveCandidate {
			// An applier seals nothing when it objects to the design (an
			// intended stop, recorded as the round's objection) or fails.
			event.Title, event.Tone = fmt.Sprintf("設計に沿った実装 %d 巡目: 変更を封緘せず (異議、または失敗)", n), "warn"
		}
	}
	if len(files) > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "変更したファイル", Text: strings.Join(files, "\n")})
	}
	report := run.Transcript
	if report == "" {
		report = candidate.Rationale
	}
	if report != "" {
		event.Evidence = append(event.Evidence, Evidence{Label: "実装役の報告", Text: shown(report)})
	}
	if run.DurationMS > 0 {
		event.Why = fmt.Sprintf("所要 %s", (time.Duration(run.DurationMS) * time.Millisecond).Round(time.Second))
	}
	v.Timeline = append(v.Timeline, event)
}

func (v *View) readReviews(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var decision struct {
		Outcome string `json:"outcome"`
	}
	readJSON(filepath.Join(dir, "decision.json"), &decision)
	for _, entry := range entries {
		name := entry.Name()
		if !isReviewRecord(name) {
			continue
		}
		reviewer := strings.TrimSuffix(name, ".json")
		var review struct {
			ReviewerID string    `json:"reviewer_id"`
			Model      string    `json:"model"`
			Verdict    string    `json:"verdict"`
			ReviewedAt time.Time `json:"reviewed_at"`
			Findings   []struct {
				Code    string `json:"code"`
				Path    string `json:"path"`
				Line    int    `json:"line"`
				Message string `json:"message"`
			} `json:"findings"`
		}
		if !readJSON(filepath.Join(dir, name), &review) || review.ReviewerID == "" || review.Verdict == "" {
			// Not a sealed verdict: the engine writes other JSON into a
			// stage (an empty first attempt of the implementer, say).
			continue
		}
		tone := "ok"
		title := fmt.Sprintf("レビュー %d 巡目 · %s: 合格", n, review.ReviewerID)
		if review.Verdict != "pass" {
			tone = "warn"
			title = fmt.Sprintf("レビュー %d 巡目 · %s: %s (指摘 %d 件)", n, review.ReviewerID, verdictWord(review.Verdict), len(review.Findings))
		}
		event := Event{At: review.ReviewedAt, Step: "review", Tone: tone, Title: title, Record: fmt.Sprintf("stage-%d-%s", n, reviewer)}
		for _, f := range review.Findings {
			where := f.Path
			if f.Line > 0 {
				where = fmt.Sprintf("%s:%d", f.Path, f.Line)
			}
			event.Evidence = append(event.Evidence, Evidence{Label: f.Code + " (" + where + ")", Text: shown(f.Message)})
		}
		if review.Model != "" {
			event.Evidence = append(event.Evidence, Evidence{Label: "モデル", Text: review.Model})
		}
		v.Timeline = append(v.Timeline, event)
	}
	// A reviewer whose run record exists without a sealed verdict did not
	// answer: the run record's transcript tail is the only evidence.
	for _, entry := range entries {
		name := entry.Name()
		reviewer, isRun := strings.CutSuffix(name, "-run.json")
		if !isRun || !isReviewRecord(reviewer+".json") || exists(filepath.Join(dir, reviewer+".json")) {
			continue
		}
		var run struct {
			AgentID    string    `json:"agent_id"`
			RanAt      time.Time `json:"ran_at"`
			Transcript string    `json:"transcript"`
		}
		if !readJSON(filepath.Join(dir, name), &run) || run.RanAt.IsZero() {
			continue
		}
		v.Timeline = append(v.Timeline, Event{
			At: run.RanAt, Step: "review", Tone: "bad", Title: fmt.Sprintf("レビュー %d 巡目 · %s: 判定を返せなかった", n, reviewer),
			Evidence: []Evidence{{Label: "レビュー役の出力の末尾", Text: shownTail(run.Transcript, 800)}},
			Record:   fmt.Sprintf("stage-%d-%s-run", n, reviewer),
		})
	}
	if decision.Outcome != "" {
		last := len(v.Timeline) - 1
		if last >= 0 && v.Timeline[last].Step == "review" {
			v.Timeline[last].Why = "この巡の結論: " + verdictWord(decision.Outcome)
		}
	}
}

// isReviewRecord says whether a stage file is a reviewer's sealed verdict:
// every JSON record of a stage that is not one of the engine's own (the
// candidate, the decision, the ticket and source copies, the implementer's
// and applier's runs) is named by a configured reviewer id.
func isReviewRecord(name string) bool {
	base, isJSON := strings.CutSuffix(name, ".json")
	if !isJSON || strings.HasSuffix(base, "-run") || !recordBase.MatchString(base) {
		return false
	}
	switch base {
	case "candidate", "decision", "ticket", "source", "implement", "implementer", "applier":
		return false
	}
	return true
}

func verdictWord(verdict string) string {
	switch verdict {
	case "pass", "converged", "accept":
		return "合格"
	case "revise":
		return "やり直し"
	case "design-wrong":
		return "設計が違う"
	}
	return verdict
}

func tail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	cut := len(text) - n
	for cut < len(text) && (text[cut]&0xC0) == 0x80 {
		cut++
	}
	return "…" + text[cut:]
}

func (v *View) readValidation(runDir string) {
	var validation struct {
		StartedAt   time.Time `json:"started_at"`
		CompletedAt time.Time `json:"completed_at"`
		Tools       []struct {
			Binary  string `json:"binary"`
			Version string `json:"version"`
		} `json:"tools"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if !readJSON(filepath.Join(runDir, "validation.json"), &validation) {
		return
	}
	var tools []string
	for _, t := range validation.Tools {
		tools = append(tools, t.Binary+" "+t.Version)
	}
	event := Event{At: validation.CompletedAt, Step: "checks", Tone: "ok", Title: "検査: 隔離サンドボックスでビルドとテストを通した", Record: "validation"}
	if !validation.StartedAt.IsZero() {
		event.Why = fmt.Sprintf("所要 %s", validation.CompletedAt.Sub(validation.StartedAt).Round(time.Second))
	}
	if len(tools) > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "道具", Text: strings.Join(tools, ", ")})
	}
	v.Timeline = append(v.Timeline, event)
}

func (v *View) readDelivery(runDir string) {
	var pr struct {
		Payload struct {
			PullRequest struct {
				Number    int       `json:"Number"`
				HTMLURL   string    `json:"HTMLURL"`
				CreatedAt time.Time `json:"CreatedAt"`
				BaseRef   string    `json:"BaseRef"`
			} `json:"pull_request"`
		} `json:"payload"`
	}
	if readJSON(filepath.Join(runDir, "feature-pr.json"), &pr) && pr.Payload.PullRequest.HTMLURL != "" {
		v.PullRequestURL = pr.Payload.PullRequest.HTMLURL
		v.Timeline = append(v.Timeline, Event{
			At: pr.Payload.PullRequest.CreatedAt, Step: "pr", Tone: "ok",
			Title:    fmt.Sprintf("PR #%d を作成 (宛先 %s)", pr.Payload.PullRequest.Number, pr.Payload.PullRequest.BaseRef),
			Evidence: []Evidence{{Label: "URL", Text: pr.Payload.PullRequest.HTMLURL}}, Record: "feature-pr",
		})
	}
	var merge struct {
		Payload struct {
			Merge struct {
				MergeSHA   string `json:"MergeSHA"`
				BaseBranch string `json:"BaseBranch"`
			} `json:"merge"`
		} `json:"payload"`
	}
	if readJSON(filepath.Join(runDir, "feature-merge.json"), &merge) && merge.Payload.Merge.MergeSHA != "" {
		v.MergeSHA = merge.Payload.Merge.MergeSHA
		v.Timeline = append(v.Timeline, Event{
			At: fileTime(filepath.Join(runDir, "feature-merge.json")), Step: "staging", Tone: "ok",
			Title:    fmt.Sprintf("取り込み: %s にマージ", merge.Payload.Merge.BaseBranch),
			Evidence: []Evidence{{Label: "マージコミット", Text: merge.Payload.Merge.MergeSHA}}, Record: "feature-merge",
		})
	}
	for _, phase := range []struct{ file, name, record string }{
		{"deliver-staging-report.json", "配布判定 (ステージング)", "deliver-staging-report"},
		{"deliver-production-report.json", "本番反映", "deliver-production-report"},
	} {
		var report struct {
			Verdict    string    `json:"verdict"`
			Detail     string    `json:"detail"`
			ObservedAt time.Time `json:"observed_at"`
			TargetURL  string    `json:"target_url"`
			MergedSHA  string    `json:"merged_sha"`
		}
		if !readJSON(filepath.Join(runDir, phase.file), &report) {
			continue
		}
		title, tone := deliverTitle(phase.name, report.Verdict)
		event := Event{At: report.ObservedAt, Step: "deliver", Tone: tone, Title: title, Why: shown(report.Detail), Record: phase.record}
		if report.TargetURL != "" {
			event.Evidence = append(event.Evidence, Evidence{Label: "確認先", Text: report.TargetURL})
		}
		v.Timeline = append(v.Timeline, event)
	}
}

func deliverTitle(phase, verdict string) (string, string) {
	switch verdict {
	case "pass":
		return phase + ": 合格", "ok"
	case "deploy_not_applicable":
		return phase + ": 配布対象外 (マージで完了)", "ok"
	case "stopped":
		return phase + ": 停止", "warn"
	case "expired":
		return phase + ": Go の期限切れ", "warn"
	case "checks_failed", "merge_failed", "deploy_failed", "observe_failed", "measure_failed", "card_failed", "promotion_failed":
		return phase + ": 不合格 (" + verdict + ")", "bad"
	case "merge_unverified", "deploy_absent", "observe_blocked":
		return phase + ": 確認が必要 (" + verdict + ")", "warn"
	}
	return phase + ": " + verdict, "neutral"
}

func (v *View) readEnding(runDir string) {
	var outcome struct {
		Phase   string    `json:"phase"`
		Verdict string    `json:"verdict"`
		At      time.Time `json:"at"`
	}
	if readJSON(filepath.Join(runDir, "board-outcome.json"), &outcome) && outcome.Verdict != "" {
		title, tone := deliverTitle("終端 ("+outcome.Phase+")", outcome.Verdict)
		v.Timeline = append(v.Timeline, Event{At: outcome.At, Step: "end", Tone: tone, Title: title + " — 票に報告済み", Record: "board-outcome"})
	}
	step, _ := os.ReadFile(filepath.Join(runDir, "failed-step.txt"))
	var detail struct {
		Step string `json:"step"`
		ModelFailure
	}
	haveDetail := readJSON(filepath.Join(runDir, "model-failure-detail.json"), &detail) && detail.Phrase != ""
	if len(step) == 0 && !haveDetail {
		return
	}
	// A reception that failed before the readiness gate (the intake above
	// all) records no failed step; the detail names the stage.
	failure := &Failure{Step: strings.TrimSpace(string(step))}
	endAt := fileTime(filepath.Join(runDir, "failed-step.txt"))
	if failure.Step == "" {
		failure.Step, endAt = shown(detail.Step), detail.RecordedAt
	}
	var record struct {
		Reason string `json:"model_failure_reason"`
	}
	if readJSON(filepath.Join(runDir, "model-failure.json"), &record) {
		failure.Reason = record.Reason
	}
	if haveDetail {
		model := detail.ModelFailure
		model.Phrase, model.Model, model.Effort, model.FinalEffort = shown(model.Phrase), shown(model.Model), shown(model.Effort), shown(model.FinalEffort)
		model.LastFinishReason, model.LastRequestID = shown(model.LastFinishReason), shown(model.LastRequestID)
		model.Objection = shown(model.Objection)
		model.Summary = modelFailureSummary(model)
		failure.Model = &model
	}
	if trail, err := os.ReadFile(filepath.Join(runDir, "m1-trail.txt")); err == nil {
		failure.Detail = shownTail(strings.TrimSpace(string(trail)), 1200)
	}
	v.Failure = failure
	why := failureWord(failure.Reason)
	if why == "" && failure.Model != nil {
		why = failure.Model.Summary
	}
	source := "failed-step"
	if len(step) == 0 {
		source = "model-failure-detail"
	}
	v.Timeline = append(v.Timeline, Event{
		At: endAt, Step: "end", Tone: "bad",
		Title: "失敗で終了: " + failure.Step, Why: why, Record: source,
	})
}

// maxRunningStepAge is how long a started step may still be called running.
// The implement stage's own bound is 90 minutes (internal/runtime chain), so
// anything older than that plus a margin is a record nobody cleared.
const maxRunningStepAge = 2 * time.Hour

// readRunning reads the step the runner started last and has not replaced.
// The record is removed when the run stops running steps, so its presence is
// what "still working" means here.
func (v *View) readRunning(runDir string) {
	v.Running = ReadRunningStep(runDir)
}

// ReadRunningStep is shared by the board and the detail page so they agree
// about a step left behind by a killed runner.
func ReadRunningStep(runDir string) *RunningStep {
	var record RunningStep
	if !readJSON(filepath.Join(runDir, "current-step.json"), &record) {
		return nil
	}
	record.Step = shown(record.Step)
	if record.Step == "" || record.StartedAt.IsZero() {
		return nil
	}
	// A runner that was killed leaves its record behind. Past the longest a
	// step may take, "still running" would be a claim nobody is making: the
	// page would pulse for ever and an unsealed round would read as in
	// progress instead of dead (review of #187).
	if time.Since(record.StartedAt) > maxRunningStepAge {
		return nil
	}
	return &record
}

// modelFailureSummary says what the recorded numbers mean, in the
// requester's words: the cases the reception's own notes distinguish.
func modelFailureSummary(d ModelFailure) string {
	switch {
	case d.LastHTTPStatus == 429 && strings.Contains(d.Phrase, worker.RetryAfterTooLongPhrase):
		return "AI の鍵が利用の上限 (429) で断られ、待つよう指定された時間が自動処理の待てる長さを超えていた"
	case d.LastHTTPStatus == 429:
		return fmt.Sprintf("AI の鍵が利用の上限 (429) で断られた (呼び出し %d 回)", d.Calls)
	case d.LastHTTPStatus >= 500 && strings.Contains(d.Phrase, worker.AttemptsRanOutPhrase):
		return fmt.Sprintf("ゲートウェイが %d を返し、聞き直しても通らなかった", d.LastHTTPStatus)
	case d.LastHTTPStatus >= 500:
		return fmt.Sprintf("ゲートウェイが %d を返した", d.LastHTTPStatus)
	case d.LastHTTPStatus > 0:
		return fmt.Sprintf("ゲートウェイが %d で断った", d.LastHTTPStatus)
	case d.LastFinishReason == "length" && d.LastReasoningTokens > 0 && d.LastReasoningTokens >= d.LastCompletionTokens:
		text := fmt.Sprintf("AI が答えを書き始める前に、考える段階だけで出力の上限 %d トークンを使い切った", d.LastCompletionTokens)
		if d.Lowered > 0 {
			text += fmt.Sprintf(" (考える深さを %d 段下げて聞き直しても同じ)", d.Lowered)
		}
		return text
	case d.LastFinishReason == "length":
		limit := d.FinalMaxOutputTokens
		if limit == 0 {
			limit = d.MaxOutputTokens
		}
		return fmt.Sprintf("AI の答えが長すぎて出力の上限 %d トークンで途切れた", limit)
	case d.LastFinishReason == "content_filter":
		return "AI が依頼文の内容を理由に答えを断った"
	case d.LastFinishReason == "error" || strings.Contains(d.Phrase, worker.ProviderEndedTurnPhrase):
		return fmt.Sprintf("モデルの提供元側の失敗で答えが返らなかった (呼び出し %d 回)", d.Calls)
	case d.AllowanceSpent > 0 && d.LastRequestID == "":
		return fmt.Sprintf("AI が制限時間内に答えを返さなかった (呼び出し %d 回)", d.Calls)
	case d.Malformed > 0 && d.Objection != "":
		return fmt.Sprintf("AI は答えたが、その答えが決められた形にならなかった (%d 回とも)。最後の答えへの指摘: %s", d.Malformed, d.Objection)
	}
	return fmt.Sprintf("AI の失敗 (呼び出し %d 回): %s", d.Calls, d.Phrase)
}

// readSpend reads the gateway's billing reading kept at the end of the run.
func (v *View) readSpend(runDir string) {
	var record struct {
		ReadAt   time.Time `json:"read_at"`
		Complete bool      `json:"complete"`
		TotalUSD float64   `json:"total_usd"`
		Keys     []struct {
			KeyEnv   string   `json:"key_env"`
			KeyName  string   `json:"key_name"`
			Roles    []string `json:"roles"`
			SpendUSD float64  `json:"spend_usd"`
			Unpriced int      `json:"unpriced_requests"`
		} `json:"keys"`
	}
	if !readJSON(filepath.Join(runDir, "spend.json"), &record) || len(record.Keys) == 0 {
		return
	}
	billed := &Billed{ReadAt: record.ReadAt, Complete: record.Complete, TotalUSD: record.TotalUSD}
	for _, key := range record.Keys {
		label := strings.Join(key.Roles, " / ")
		if label == "" {
			label = key.KeyName
		}
		if label == "" {
			label = key.KeyEnv
		}
		// The key's name is the gateway's word, not ours: scanned like any
		// other text before it is shown.
		billed.Lines = append(billed.Lines, BilledLine{Label: shown(label), KeyName: shown(key.KeyName), USD: key.SpendUSD, Unpriced: key.Unpriced})
	}
	v.Cost.Billed = billed
}

func failureWord(reason string) string {
	switch reason {
	case "":
		return ""
	case hook.ModelFailureBudgetExhausted:
		return "AI の鍵の利用枠 (予算) が上限に達した"
	}
	return "AI の失敗: " + reason
}

func (v *View) readMeasurements(runDir string) {
	raw, err := os.ReadFile(filepath.Join(runDir, "measurements.jsonl"))
	if err != nil {
		return
	}
	var lines []string
	var first time.Time
	refused := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m struct {
			ID        string            `json:"id"`
			Probe     string            `json:"probe"`
			Args      map[string]string `json:"args"`
			StartedAt time.Time         `json:"started_at"`
			Refused   bool              `json:"refused"`
			Reason    string            `json:"reason"`
			Masked    []string          `json:"masked"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		if first.IsZero() {
			first = m.StartedAt
		}
		var args []string
		for k, val := range m.Args {
			args = append(args, k+"="+val)
		}
		sort.Strings(args)
		entry := m.ID + " " + m.Probe + " " + strings.Join(args, " ")
		if m.Refused {
			refused++
			entry += " — 拒否: " + m.Reason
		} else if len(m.Masked) > 0 {
			entry += " — 伏せ字: " + strings.Join(m.Masked, ", ")
		}
		lines = append(lines, shown(entry))
	}
	if len(lines) == 0 {
		return
	}
	v.Timeline = append(v.Timeline, Event{
		At: first, Step: "investigate", Tone: "neutral",
		Title:    fmt.Sprintf("調査・設計: 計測 %d 件 (うち拒否 %d 件)", len(lines), refused),
		Evidence: []Evidence{{Label: "計測の一覧", Text: strings.Join(lines, "\n")}}, Record: "measurements",
	})
}
