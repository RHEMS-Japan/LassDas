package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
	"net/http"
)

// Terminal closes a run the way cmd/reporter and cmd/questioner did over
// HTTP, but in process: the same request structs, the same shape gates
// (Marshal* before submission), the same retry-on-contention loop. The
// identity block is not re-derived from the environment — it is the exact
// PullOwner this run claimed with, so the sealed run item and the closing
// report can only agree or the store refuses.
type Terminal struct {
	config      runtime.Config
	services    *runtime.Services
	envelope    hook.DispatchEnvelope
	hermesRunID int64
	workspace   string
	logger      interface {
		Info(string, ...any)
		Error(string, ...any)
	}
	// spendTransport lets package tests stand in for the billing endpoints
	// the cost line is read from; nil means the real network. Production
	// never sets it.
	spendTransport http.RoundTripper
}

func NewTerminal(config runtime.Config, services *runtime.Services, envelope hook.DispatchEnvelope, hermesRunID int64, workspace string, logger interface {
	Info(string, ...any)
	Error(string, ...any)
}) *Terminal {
	return &Terminal{config: config, services: services, envelope: envelope,
		hermesRunID: hermesRunID, workspace: workspace, logger: logger}
}

// runURL is this run's sealed reference in the pod constitution: the same
// three identities the GitHub URL carried (repository, run id, attempt),
// in the local-run scheme validRunURL accepts.
func (t *Terminal) runURL(owner hook.PullOwner) string {
	return "local-run://" + t.config.Identity.Repository + "/" +
		strconv.FormatInt(owner.WorkflowRunID, 10) + "/attempts/" + strconv.Itoa(owner.RunAttempt)
}

const (
	terminalSubmitAttempts = 3
	// The Lambda waited 125s between report attempts because its contention
	// was another cold-started invocation; here contention is the attendant's
	// tick holding the single-writer ledger lock, which clears in seconds.
	terminalRetryDelay = 3 * time.Second
)

var terminalRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// Report submits the terminal outcome for this run. repository is the
// consumer repository the run delivered to ("" when the run failed before
// any repository work — the protocol accepts that explicitly).
func (t *Terminal) Report(ctx context.Context, code hook.TerminalCode, outcome Outcome, repository string) error {
	report, err := t.buildReport(ctx, code, outcome, repository, true)
	if err != nil {
		return err
	}
	// Whatever the step was, written down here rather than at each caller.
	// Three callers each wrote it themselves and two of the three were free
	// to delete with every test still green — twice, on the same asymmetry
	// (review of #132). Every report goes through this one line.
	recordFailedStep(t.workspace, outcome.Evidence)
	if err := t.submit(ctx, "terminal report", func(issuedAt time.Time) (hook.Result, error) {
		report.IssuedAt = issuedAt
		if _, err := hook.MarshalTerminalReportRequest(report); err != nil {
			return hook.Result{}, fmt.Errorf("terminal report shape invalid: %w", err)
		}
		return t.services.Report.ProcessTerminalReport(ctx, report), nil
	}); err != nil {
		return err
	}
	// The run is closed; what the requester decided along the way is kept
	// for the next one, whatever code this report carried.
	t.preserveAnswers()
	// Closed also means the destination clones have no reader left.
	t.pruneRunClones()
	return nil
}

// ReportDigest is the digest the store would seal for this outcome — the
// record's immutable fields, without the attempt timestamp, the trail or
// the spend line. A re-submission of a pending report compares it with the
// digest the row was begun with before choosing what to send.
func (t *Terminal) ReportDigest(ctx context.Context, code hook.TerminalCode, outcome Outcome, repository string) (string, error) {
	report, err := t.buildReport(ctx, code, outcome, repository, false)
	if err != nil {
		return "", err
	}
	// The record excludes the attempt timestamp; the shape check still wants
	// one, as every attempt carries one.
	report.IssuedAt = time.Now().UTC()
	record, err := hook.MarshalTerminalReportRecord(report)
	if err != nil {
		return "", fmt.Errorf("terminal report shape invalid: %w", err)
	}
	return hook.TerminalReportDigest(record), nil
}

// buildReport assembles the request the way cmd/reporter did. withSpend
// says whether to read the live spend line; the digest does not include it,
// so a digest-only build skips the gateway read.
func (t *Terminal) buildReport(ctx context.Context, code hook.TerminalCode, outcome Outcome, repository string, withSpend bool) (hook.TerminalReportRequest, error) {
	if repository != "" && !terminalRepositoryPattern.MatchString(repository) {
		return hook.TerminalReportRequest{}, fmt.Errorf("consumer repository %q is not owner/name", repository)
	}
	trail, err := t.loadTrail(code)
	if err != nil {
		return hook.TerminalReportRequest{}, err
	}
	owner, err := t.owner(ctx)
	if err != nil {
		return hook.TerminalReportRequest{}, err
	}
	evidence := outcome.Evidence
	report := hook.TerminalReportRequest{
		Protocol:   hook.TerminalReportProtocolVersion,
		DeliveryID: t.envelope.DeliveryID, InputSHA256: t.envelope.Snapshot.InputSHA256,
		RepositoryID: owner.RepositoryID, RepositorySHA256: owner.RepositorySHA256,
		WorkflowRefSHA256: owner.WorkflowRefSHA256, WorkflowSHA: owner.WorkflowSHA,
		WorkflowRunID: owner.WorkflowRunID, RunAttempt: owner.RunAttempt,
		AutomationRunID: t.envelope.Snapshot.RunID, Code: code, Repository: repository,
		RunURL:         t.runURL(owner),
		PullRequestURL: evidence["pull_request_url"], CommitSHA: evidence["commit_sha"],
		CommitURL: evidence["commit_url"], StagingEvidenceURL: evidence["staging_evidence_url"],
		ProductionEvidenceURL: evidence["production_evidence_url"],
		IncompleteReason:      evidence["incomplete_reason"],
		IncompleteObjection:   evidence["incomplete_objection"],
		FailedStep:            evidence["failed_step"],
		ModelFailureReason:    evidence["model_failure_reason"],
		// How far this delivery actually went, and what a deeper one would
		// have needed. Both travel in the evidence map like every other
		// report field the chain assembles.
		ReachedDelivery:   evidence["reached_delivery"],
		DeliveryShortfall: evidence["delivery_shortfall"],
	}
	if withSpend {
		report.SpendText = t.loadRunSpendText(ctx)
		// What the delivery made possible, and what was decided along the
		// way without asking. Neither is part of the sealed record, so a
		// digest-only build skips both reads: a pending report resubmitted
		// every tick would otherwise walk the whole run directory each time
		// to compute something the digest does not contain.
		report.OutcomeText, report.AssumptionsText = composeOutcome(t.workspace, code, evidence)
	}
	fitReportText(&report, trail)
	return report, nil
}

// fitReportText decides how much of this report's prose the envelope can
// carry, and drops what it cannot in the order the parts are worth least to
// the person reading.
//
// The envelope is the one bound in this chain that cannot be given way: a
// report too large to marshal is a delivery that ends saying nothing at all.
// So the account of how the work went gives way first, then what was decided
// on the requester's behalf, and the outcome last of all — which is the part
// they came for and the part that stays.
//
// The room is found by measuring rather than by arithmetic. What a string
// costs in the envelope is not its length: the encoder escapes, and a record
// full of quotes and angle brackets costs several times what counting its
// bytes would suggest.
func fitReportText(report *hook.TerminalReportRequest, trail string) {
	// The shape check wants a timestamp, as every attempt carries one; the
	// real one is stamped per attempt and is the same length as this.
	probe := *report
	probe.IssuedAt = time.Now().UTC()
	// The composer measures the envelope itself. Marshalling only gates the
	// report's shape; the size is refused where the request is decoded, which
	// is far enough downstream that a report too large would be discovered by
	// not arriving.
	fits := func() bool {
		encoded, err := hook.MarshalTerminalReportRequest(probe)
		return err == nil && len(encoded) <= hook.MaxTerminalReportRequestBytes
	}
	room := hook.MaxTerminalTrailBytes
	for attempt := 0; attempt < terminalTrailFitAttempts; attempt++ {
		probe.TrailText = hook.ShortenTrailForComment(trail, room)
		if fits() {
			report.TrailText = probe.TrailText
			return
		}
		room /= 2
	}
	probe.TrailText = ""
	report.TrailText = ""
	if fits() {
		return
	}
	probe.AssumptionsText = ""
	report.AssumptionsText = ""
	if fits() {
		return
	}
	probe.OutcomeText = ""
	report.OutcomeText = ""
}

// terminalTrailFitAttempts halves the record's budget this many times before
// giving up on carrying any of it. Eight halvings take the eight-kilobyte
// budget below a single byte, so the loop cannot end with room to spare.
const terminalTrailFitAttempts = 8

// owner is the identity this run's reports are bound to: the one the run
// was claimed under, read from the ledger's run row. The engine that ends a
// run is not always the engine that claimed it — a release in between
// changes Identity.EngineSHA — and the store refuses a terminal report or
// question whose owner differs from the claim (terminal_report_conflict,
// live 2026-09-05). A row that cannot be read is an error, not a reason to
// report under the current identity: that would recreate the refusal, and
// an owner that changed between attempts would turn a retry into a conflict.
// The run stays claimed and the next tick drives it again. Only a run with
// no claim row (or no store, in tests) reports under the current identity,
// which the store then judges on its own.
//
// The row holds whoever owns the claim now, and that is not always this
// execution. When a claim is handed back to the queue as lost and a fresh
// execution claims it, an earlier execution that turns out to be alive
// would read the newer one's identity here and end the run wearing it.
// That is what happened live on 2026-09-24: an earlier execution finished,
// read the identity of the execution that had replaced it, and posted a
// terminal comment saying the delivery had succeeded and citing the
// replacement's run reference — while the replacement was still running,
// and went on to fail. The requester was told one execution's outcome
// under another execution's name. So the claim is taken only while it is
// still this execution's; a claim that has moved on ends the call with an
// error, nothing is posted, nothing in the ledger changes, and the
// execution that does own the run is left to report for it.
//
// Both orchestrations pass the identity they claimed with. A dispatched
// worker carries its own dispatch's run id, which is exactly why a
// re-dispatch of the same card is a different execution to compare
// against. The attendant carries an id derived from the delivery id
// (chainOwnerRunID), so its reports are the same identity on every tick,
// across process restarts and across a recovered claim: it keeps one
// delivery rather than being one execution of it, and this check never
// stands in its way.
func (t *Terminal) owner(ctx context.Context) (hook.PullOwner, error) {
	mine := t.config.Owner(t.hermesRunID)
	if t.services == nil || t.services.Store == nil {
		return mine, nil
	}
	route := t.services.Route
	route.ExpectedRunID = t.envelope.Snapshot.RunID
	// A transient ledger read failure is retried the way the report itself
	// is (terminalSubmitAttempts × terminalRetryDelay); the row's values do
	// not change between attempts, so the report digest stays the same.
	var err error
	for attempt := 1; attempt <= terminalSubmitAttempts; attempt++ {
		var claimed hook.PullOwner
		var found bool
		claimed, found, err = t.services.Store.ClaimOwner(ctx, route)
		if err == nil {
			if !found {
				return mine, nil
			}
			if difference := ownerDifference(mine, claimed); difference != "" {
				return hook.PullOwner{}, fmt.Errorf(
					"claim owner changed: %s; this execution no longer owns the run and reports nothing for it", difference)
			}
			return claimed, nil
		}
		if attempt < terminalSubmitAttempts {
			select {
			case <-ctx.Done():
				return hook.PullOwner{}, fmt.Errorf("claim owner unreadable: %w", ctx.Err())
			case <-time.After(terminalRetryDelay):
			}
		}
	}
	return hook.PullOwner{}, fmt.Errorf("claim owner unreadable: %w", err)
}

// ownerDifference says how the claim's owner block differs from this
// execution's own, or "" when the two are the same execution. The engine
// revision is the single field allowed to move under a running delivery,
// so it is copied across before the blocks are compared whole: everything
// else — the engine repository, the workflow, the run id, the attempt —
// has to match, and a field added to the block later is strict by default
// rather than quietly permitted.
func ownerDifference(mine, claimed hook.PullOwner) string {
	mine.WorkflowSHA = claimed.WorkflowSHA
	if mine == claimed {
		return ""
	}
	switch {
	case claimed.WorkflowRunID != mine.WorkflowRunID:
		return fmt.Sprintf("the run is claimed by execution %d, not %d", claimed.WorkflowRunID, mine.WorkflowRunID)
	case claimed.RunAttempt != mine.RunAttempt:
		return fmt.Sprintf("the run is claimed by attempt %d, not %d", claimed.RunAttempt, mine.RunAttempt)
	case claimed.RepositoryID != mine.RepositoryID || claimed.RepositorySHA256 != mine.RepositorySHA256:
		return "the run is claimed under another engine repository"
	case claimed.WorkflowRefSHA256 != mine.WorkflowRefSHA256:
		return "the run is claimed under another workflow"
	}
	return "the run is claimed under another identity"
}

// loadTrail reads the delivery trail when the run composed one. The
// workflow attached the trail whenever the file existed — including
// post-publish failures like release_failed, whose reports carry the run
// record of a real repository change. cmd/reporter treated an unreadable
// trail as fatal and so does this: the run stays claimed and the recovery
// path surfaces it, rather than a report being sealed without the trail
// that explains it.
//
// The record on disk is the whole thing; the report envelope carries only
// what one ticket comment can hold, so a longer record is shortened here and
// says so. A record over the comment's size used to make the file "invalid"
// and end the run with no report at all, which is the opposite of what a
// fuller record should cost.
func (t *Terminal) loadTrail(hook.TerminalCode) (string, error) {
	path := t.workspace + "/m1-trail.txt"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(hook.MaxTrailRecordBytes) {
		return "", errors.New("trail file invalid")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("trail unreadable: %w", err)
	}
	if hook.ValidateTrailTextWithin(string(encoded), hook.MaxTrailRecordBytes) != nil {
		return "", errors.New("trail text invalid")
	}
	return hook.ShortenTrailForComment(string(encoded), hook.MaxTerminalTrailBytes), nil
}

// AskQuestion posts the clarification decision the model stage produced.
// The decision file is validated exactly as cmd/questioner validated it,
// and the record derives its round from the sealed clarification in the
// envelope — never from a counter the runner keeps.
func (t *Terminal) AskQuestion(ctx context.Context, decisionPath string) error {
	if t == nil || t.services == nil || t.services.Question == nil {
		// A deployment without the question poster cannot ask; saying so
		// beats crashing the process that was trying to.
		return errors.New("this deployment has no question poster")
	}
	questionsJSON, decisionDigest, err := loadQuestionDecision(decisionPath)
	if err != nil {
		return err
	}
	revision, clarificationDigest, err := questionRevision(t.envelope)
	if err != nil {
		return err
	}
	owner, err := t.owner(ctx)
	if err != nil {
		return err
	}
	// The schedule is sealed once, exactly as cmd/questioner computed it
	// once and retried the same record: recomputing per retry could change
	// the record digest across a day boundary and turn an idempotent
	// completion into a conflict.
	notifyAt, deadlineAt := hook.ComputeQuestionScheduleWithin(time.Now().UTC(), t.answerWeekdays())
	return t.submit(ctx, "question", func(issuedAt time.Time) (hook.Result, error) {
		record := hook.QuestionRecord{
			Protocol:   hook.QuestionProtocolVersion,
			DeliveryID: t.envelope.DeliveryID, InputSHA256: t.envelope.Snapshot.InputSHA256,
			RepositoryID: owner.RepositoryID, RepositorySHA256: owner.RepositorySHA256,
			WorkflowRefSHA256: owner.WorkflowRefSHA256, WorkflowSHA: owner.WorkflowSHA,
			WorkflowRunID: owner.WorkflowRunID, RunAttempt: owner.RunAttempt,
			AutomationRunID:     t.envelope.Snapshot.RunID,
			RunURL:              t.runURL(owner),
			QuestionRevision:    revision,
			ClarificationSHA256: clarificationDigest,
			QuestionsJSON:       questionsJSON,
			QuestionsSHA256:     hook.TerminalReportDigest([]byte(questionsJSON)),
			DecisionSHA256:      decisionDigest,
			AnswerDeadlineAt:    deadlineAt,
			NotifyAt:            notifyAt,
		}
		if _, err := hook.MarshalQuestionRecord(record); err != nil {
			return hook.Result{}, fmt.Errorf("question record shape invalid: %w", err)
		}
		return t.services.Question.ProcessQuestionReport(ctx, hook.QuestionReportRequest{
			Record: record, IssuedAt: issuedAt,
		}), nil
	})
}

// answerWeekdays is how long the requester gets to answer, as the
// destination set it. A configuration that cannot be read is not a reason to
// leave the question unasked: the requester still has to see it, so the
// standard window stands and the posting goes on.
func (t *Terminal) answerWeekdays() int {
	config, err := worker.LoadConfig(t.config.ConsumerConfigPath)
	if err != nil {
		return hook.DefaultQuestionDeadlineWeekdays
	}
	return config.AnswerWeekdays()
}

// submit runs one build-and-process closure with the reporter's retry
// contract: contention retries, everything else is final. "accepted" and
// "ignored" both close the run — ignored is the idempotent replay of a
// report the store already sealed.
func (t *Terminal) submit(ctx context.Context, kind string, attempt func(time.Time) (hook.Result, error)) error {
	for round := 0; round < terminalSubmitAttempts; round++ {
		result, err := attempt(time.Now().UTC())
		if err != nil {
			return err
		}
		switch result.Decision {
		case hook.DecisionAccepted, hook.DecisionIgnored:
			t.logger.Info(kind+" sealed", "decision", string(result.Decision), "code", result.Code)
			return nil
		case hook.DecisionRetryRequested, hook.DecisionDependencyFailed, hook.DecisionInternal:
			t.logger.Error(kind+" deferred", "decision", string(result.Decision), "code", result.Code)
			if round == terminalSubmitAttempts-1 {
				return fmt.Errorf("%s not sealed after %d attempts: %s (%s)", kind, terminalSubmitAttempts, result.Decision, result.Code)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(terminalRetryDelay):
			}
		default:
			return fmt.Errorf("%s refused: %s (%s)", kind, result.Decision, result.Code)
		}
	}
	return fmt.Errorf("%s not sealed", kind)
}

// loadQuestionDecision is cmd/questioner's loadDecision: the decision file
// must be a clarification_required outcome with questions and its own
// digest, and only the questions array travels into the record.
func loadQuestionDecision(filePath string) (string, string, error) {
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256*1024 {
		return "", "", errors.New("question decision file invalid")
	}
	encoded, err := os.ReadFile(filePath)
	if err != nil {
		return "", "", errors.New("question decision file invalid")
	}
	var decision struct {
		Outcome        string           `json:"outcome"`
		Questions      []map[string]any `json:"questions"`
		DecisionSHA256 string           `json:"decision_sha256"`
	}
	if err := json.Unmarshal(encoded, &decision); err != nil {
		return "", "", errors.New("question decision file invalid")
	}
	if decision.Outcome != "clarification_required" || len(decision.Questions) == 0 ||
		!questionDigestPattern.MatchString(decision.DecisionSHA256) {
		return "", "", errors.New("question decision file invalid")
	}
	questions, err := json.Marshal(decision.Questions)
	if err != nil {
		return "", "", errors.New("question decision file invalid")
	}
	return string(questions), decision.DecisionSHA256, nil
}

var questionDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// questionRevision is cmd/questioner's questionRevisionFromEnvelope: round 1
// for a never-resumed run, otherwise the round after the sealed adopted
// answers, chained to them by digest.
func questionRevision(envelope hook.DispatchEnvelope) (int, string, error) {
	if envelope.ClarificationJSON == "" {
		return 1, "", nil
	}
	record, err := hook.DecodeClarificationRecord([]byte(envelope.ClarificationJSON))
	if err != nil {
		return 0, "", errors.New("sealed clarification invalid")
	}
	if record.InputRevision > hook.MaxClarificationRounds {
		return 0, "", errors.New("question rounds exhausted")
	}
	return record.InputRevision, hook.TerminalReportDigest([]byte(envelope.ClarificationJSON)), nil
}
