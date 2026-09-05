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
		TrailText:             trail,
	}
	if withSpend {
		report.SpendText = t.loadRunSpendText(ctx)
	}
	return report, nil
}

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
func (t *Terminal) owner(ctx context.Context) (hook.PullOwner, error) {
	fallback := t.config.Owner(t.hermesRunID)
	if t.services == nil || t.services.Store == nil {
		return fallback, nil
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
				return fallback, nil
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

// loadTrail reads the delivery trail when the run composed one. The
// workflow attached the trail whenever the file existed — including
// post-publish failures like release_failed, whose reports carry the run
// record of a real repository change. cmd/reporter treated an unreadable
// trail as fatal and so does this: the run stays claimed and the recovery
// path surfaces it, rather than a report being sealed without the trail
// that explains it.
func (t *Terminal) loadTrail(hook.TerminalCode) (string, error) {
	path := t.workspace + "/m1-trail.txt"
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > int64(hook.MaxTerminalTrailBytes) {
		return "", errors.New("trail file invalid")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("trail unreadable: %w", err)
	}
	if hook.ValidateTrailText(string(encoded)) != nil {
		return "", errors.New("trail text invalid")
	}
	return string(encoded), nil
}

// AskQuestion posts the clarification decision the model stage produced.
// The decision file is validated exactly as cmd/questioner validated it,
// and the record derives its round from the sealed clarification in the
// envelope — never from a counter the runner keeps.
func (t *Terminal) AskQuestion(ctx context.Context, decisionPath string) error {
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
	notifyAt, deadlineAt := hook.ComputeQuestionSchedule(time.Now().UTC())
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
