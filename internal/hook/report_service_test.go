package hook

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type terminalFakeStore struct {
	beginBindings     []TerminalBinding
	beginDispositions []TerminalBeginDisposition
	beginErrors       []error
	completeResults   []TerminalCompleteDisposition
	completeErrors    []error
	beginRequests     []TerminalBeginRequest
	completeRequests  []TerminalCompleteRequest
}

func (f *terminalFakeStore) BeginTerminal(_ context.Context, request TerminalBeginRequest) (TerminalBinding, TerminalBeginDisposition, error) {
	f.beginRequests = append(f.beginRequests, request)
	index := len(f.beginRequests) - 1
	return valueAt(f.beginBindings, index), valueAt(f.beginDispositions, index), valueAt(f.beginErrors, index)
}

func (f *terminalFakeStore) CompleteTerminal(_ context.Context, request TerminalCompleteRequest) (TerminalCompleteDisposition, error) {
	f.completeRequests = append(f.completeRequests, request)
	index := len(f.completeRequests) - 1
	return valueAt(f.completeResults, index), valueAt(f.completeErrors, index)
}

type terminalFakeComments struct {
	findIDs       []int64
	findResults   []bool
	findErrors    []error
	addIDs        []int64
	addErrors     []error
	findContents  []string
	addContents   []string
	addedIDs      []int64
	markerLookups []string
}

func (f *terminalFakeComments) FindExactComment(_ context.Context, _ int64, content string) (int64, bool, error) {
	f.findContents = append(f.findContents, content)
	index := len(f.findContents) - 1
	return valueAt(f.findIDs, index), valueAt(f.findResults, index), valueAt(f.findErrors, index)
}

func (f *terminalFakeComments) FindCommentWithMarker(_ context.Context, _ int64, marker string) (int64, bool, error) {
	f.markerLookups = append(f.markerLookups, marker)
	for i, content := range f.addContents {
		if strings.Contains(content, marker) && i < len(f.addedIDs) {
			return f.addedIDs[i], true, nil
		}
	}
	return 0, false, nil
}

func (f *terminalFakeComments) AddComment(_ context.Context, _ int64, content string) (int64, error) {
	f.addContents = append(f.addContents, content)
	index := len(f.addContents) - 1
	id := valueAt(f.addIDs, index)
	f.addedIDs = append(f.addedIDs, id)
	return id, valueAt(f.addErrors, index)
}

func valueAt[T any](values []T, index int) T {
	var zero T
	if index < 0 || index >= len(values) {
		return zero
	}
	return values[index]
}

func newTerminalTestService(t *testing.T, store *terminalFakeStore, comments *terminalFakeComments, logBuffer *bytes.Buffer) *TerminalReportService {
	t.Helper()
	if store == nil {
		store = &terminalFakeStore{}
	}
	if comments == nil {
		comments = &terminalFakeComments{}
	}
	if logBuffer == nil {
		logBuffer = &bytes.Buffer{}
	}
	service, err := NewTerminalReportService(terminalTestConfig(), store, comments, slog.New(slog.NewJSONHandler(logBuffer, nil)))
	if err != nil {
		t.Fatalf("NewTerminalReportService() error = %v", err)
	}
	service.now = func() time.Time { return functionURLTestNow }
	service.token = func() (string, error) { return strings.Repeat("a", 32), nil }
	return service
}

func TestTerminalReportServicePostsOnlyFixedCommentAndCompletesOutbox(t *testing.T) {
	store := &terminalFakeStore{
		beginBindings:     []TerminalBinding{{IssueID: 404, IssueKey: "TICKET-505"}},
		beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
		completeResults:   []TerminalCompleteDisposition{TerminalCompleted},
	}
	comments := &terminalFakeComments{addIDs: []int64{808}}
	report := terminalTestRequest(TerminalSuccess)
	result := newTerminalTestService(t, store, comments, nil).ProcessTerminalReport(context.Background(), report)
	if result.Decision != DecisionAccepted || result.Code != "terminal_report_recorded" {
		t.Fatalf("result = %+v", result)
	}
	if len(comments.findContents) != 1 || len(comments.addContents) != 1 || comments.findContents[0] != comments.addContents[0] {
		t.Fatalf("comment calls: find=%d add=%d", len(comments.findContents), len(comments.addContents))
	}
	comment := comments.addContents[0]
	for _, expected := range []string{string(TerminalSuccess), report.RunURL, report.PullRequestURL, report.CommitURL, report.StagingEvidenceURL, report.ProductionEvidenceURL, "[ticket-automation:v1:terminal:"} {
		if !strings.Contains(comment, expected) {
			t.Fatalf("fixed comment is missing %q: %q", expected, comment)
		}
	}
	if len(store.completeRequests) != 1 || store.completeRequests[0].CommentID != 808 ||
		store.completeRequests[0].ReportSHA256 != store.beginRequests[0].ReportSHA256 {
		t.Fatalf("complete request did not preserve the report binding: %+v", store.completeRequests)
	}
}

func TestProductionVerificationFailureCommentStatesThatProductionWasDeployed(t *testing.T) {
	report := terminalTestRequest(TerminalProductionVerificationFailed)
	comment := fixedTerminalComment(report, strings.Repeat("f", 64))
	for _, expected := range []string{
		string(TerminalProductionVerificationFailed), "本番デプロイは完了しました", report.PullRequestURL,
		report.CommitURL, report.StagingEvidenceURL,
	} {
		if !strings.Contains(comment, expected) {
			t.Fatalf("production verification comment is missing %q: %q", expected, comment)
		}
	}
	if strings.Contains(comment, "production確認先:") {
		t.Fatal("failed production verification claimed a production evidence URL")
	}
}

func TestProductionDeploymentUnverifiedCommentStatesThatProdWasMerged(t *testing.T) {
	report := terminalTestRequest(TerminalProductionDeploymentUnverified)
	comment := fixedTerminalComment(report, strings.Repeat("f", 64))
	for _, expected := range []string{
		string(TerminalProductionDeploymentUnverified), "prodブランチへの反映は完了しました",
		report.PullRequestURL, report.CommitURL, report.StagingEvidenceURL,
	} {
		if !strings.Contains(comment, expected) {
			t.Fatalf("production deployment uncertainty comment is missing %q: %q", expected, comment)
		}
	}
	if strings.Contains(comment, "production確認先:") {
		t.Fatal("unverified production deployment claimed a production evidence URL")
	}
}

func TestTerminalReportRetryFindsExistingCommentInsteadOfPostingDuplicate(t *testing.T) {
	store := &terminalFakeStore{
		beginBindings: []TerminalBinding{
			{IssueID: 404, IssueKey: "TICKET-505"},
			{IssueID: 404, IssueKey: "TICKET-505"},
		},
		beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired, TerminalBeginAcquired},
		completeResults:   []TerminalCompleteDisposition{"", TerminalCompleted},
		completeErrors:    []error{NewExternalFailure("dynamodb", FailureRetryable, "write_failed"), nil},
	}
	comments := &terminalFakeComments{
		findIDs:     []int64{0, 808},
		findResults: []bool{false, true},
		addIDs:      []int64{808},
	}
	service := newTerminalTestService(t, store, comments, nil)
	report := terminalTestRequest(TerminalValidationFailed)
	first := service.ProcessTerminalReport(context.Background(), report)
	retryReport := report
	retryReport.IssuedAt = report.IssuedAt.Add(time.Minute)
	second := service.ProcessTerminalReport(context.Background(), retryReport)
	if first.Decision != DecisionRetryRequested || second.Decision != DecisionAccepted {
		t.Fatalf("results: first=%+v second=%+v", first, second)
	}
	// The retry recognises the posted comment by its marker before any
	// exact-content lookup, so the exact lookup ran once (the first attempt).
	if len(comments.addContents) != 1 || len(comments.findContents) != 1 || len(comments.markerLookups) != 2 {
		t.Fatalf("retry posted a duplicate: find=%d marker=%d add=%d", len(comments.findContents), len(comments.markerLookups), len(comments.addContents))
	}
	if store.beginRequests[0].ReportSHA256 != store.beginRequests[1].ReportSHA256 || store.beginRequests[0].ReportJSON != store.beginRequests[1].ReportJSON {
		t.Fatal("fresh retry timestamp changed the immutable terminal outcome")
	}
}

func TestTerminalReportPendingAndCompleteDoNotCallBacklog(t *testing.T) {
	for _, disposition := range []TerminalBeginDisposition{TerminalBeginBusy, TerminalBeginComplete, TerminalBeginConflict} {
		t.Run(string(disposition), func(t *testing.T) {
			store := &terminalFakeStore{beginBindings: []TerminalBinding{{IssueID: 404}}, beginDispositions: []TerminalBeginDisposition{disposition}}
			comments := &terminalFakeComments{}
			result := newTerminalTestService(t, store, comments, nil).ProcessTerminalReport(context.Background(), terminalTestRequest(TerminalNonconverged))
			if len(comments.findContents) != 0 || len(comments.addContents) != 0 {
				t.Fatal("Backlog was called without an acquired outbox lease")
			}
			if disposition == TerminalBeginBusy && result.Decision != DecisionRetryRequested {
				t.Fatalf("busy result = %+v", result)
			}
			if disposition == TerminalBeginComplete && result.Decision != DecisionAccepted {
				t.Fatalf("complete result = %+v", result)
			}
			if disposition == TerminalBeginConflict && result.Code != "terminal_report_conflict" {
				t.Fatalf("conflict result = %+v", result)
			}
		})
	}
}

func TestTerminalReportNeverCopiesTicketOrDependencyErrorsIntoCommentOrResult(t *testing.T) {
	const sentinel = "UNTRUSTED-TICKET-AND-SECRET-SENTINEL"
	store := &terminalFakeStore{
		beginBindings:     []TerminalBinding{{IssueID: 404}},
		beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired},
	}
	comments := &terminalFakeComments{findErrors: []error{errors.New(sentinel)}}
	logs := &bytes.Buffer{}
	result := newTerminalTestService(t, store, comments, logs).ProcessTerminalReport(context.Background(), terminalTestRequest(TerminalInternalFailed))
	if strings.Contains(result.Code, sentinel) || strings.Contains(logs.String(), sentinel) {
		t.Fatalf("dependency error leaked: result=%+v logs=%q", result, logs.String())
	}
	comment := fixedTerminalComment(terminalTestRequest(TerminalInternalFailed), strings.Repeat("f", 64))
	if strings.Contains(comment, sentinel) {
		t.Fatalf("fixed comment copied untrusted text: %q", comment)
	}
}

func TestTerminalReportServiceRejectsInvalidRouteBeforeStateOrBacklog(t *testing.T) {
	store := &terminalFakeStore{}
	comments := &terminalFakeComments{}
	report := terminalTestRequest(TerminalSuccess)
	report.RunURL = "https://attacker.invalid/run"
	result := newTerminalTestService(t, store, comments, nil).ProcessTerminalReport(context.Background(), report)
	if result.Decision != DecisionInvalid || len(store.beginRequests) != 0 || len(comments.findContents) != 0 {
		t.Fatalf("invalid route reached dependencies: result=%+v begin=%d comments=%d", result, len(store.beginRequests), len(comments.findContents))
	}
}

func TestEveryFiniteTerminalCodeHasADedicatedUserFacingMessage(t *testing.T) {
	const fallback = "自動処理は終了しました。詳細は実行履歴を参照してください。"
	for _, code := range []TerminalCode{
		TerminalSuccess, TerminalInputRejected, TerminalReadinessRejected, TerminalClarificationRequired,
		TerminalReadinessUnresolved, TerminalClarificationExpired, TerminalCancelled,
		TerminalModelFailed, TerminalNonconverged,
		TerminalValidationFailed, TerminalReleaseFailed, TerminalProductionDeploymentUnverified,
		TerminalProductionVerificationFailed, TerminalInternalFailed,
	} {
		comment := fixedTerminalComment(terminalTestRequest(code), strings.Repeat("f", 64))
		if strings.Contains(comment, fallback) {
			t.Fatalf("code %q fell back to the generic message", code)
		}
	}
}

// A re-submitted report whose comment body drifted — the spend line is read
// live, the trail may have grown — is recognised by its marker: no second
// final-result comment is posted and the completion binds the posted one.
func TestTerminalReportRetryFindsTheCommentByMarkerWhenTheBodyDrifted(t *testing.T) {
	store := &terminalFakeStore{
		beginBindings:     []TerminalBinding{{IssueID: 404, IssueKey: "TICKET-505"}, {IssueID: 404, IssueKey: "TICKET-505"}},
		beginDispositions: []TerminalBeginDisposition{TerminalBeginAcquired, TerminalBeginAcquired},
		completeResults:   []TerminalCompleteDisposition{TerminalCompleted, TerminalCompleted},
	}
	comments := &terminalFakeComments{addIDs: []int64{808, 909}}
	service := newTerminalTestService(t, store, comments, nil)
	first := terminalTestRequest(TerminalModelFailed)
	first.SpendText = "合計: $0.05"
	if result := service.ProcessTerminalReport(context.Background(), first); result.Decision != DecisionAccepted {
		t.Fatalf("first report: %+v", result)
	}
	second := first
	second.SpendText = "合計: $0.07"
	if result := service.ProcessTerminalReport(context.Background(), second); result.Decision != DecisionAccepted {
		t.Fatalf("second report: %+v", result)
	}
	if len(comments.addContents) != 1 {
		t.Fatalf("a second final-result comment was posted: %d comments", len(comments.addContents))
	}
	if len(comments.markerLookups) != 2 || !strings.HasPrefix(comments.markerLookups[1], "[ticket-automation:v1:terminal:") {
		t.Fatalf("marker lookups = %v", comments.markerLookups)
	}
	if len(store.completeRequests) != 2 || store.completeRequests[1].CommentID != 808 {
		t.Fatalf("the completion did not bind the posted comment: %+v", store.completeRequests)
	}
}
