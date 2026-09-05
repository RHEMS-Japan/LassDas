package runner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

const (
	answersTestRepository  = "example/engine"
	answersTestWorkflowRef = "example/engine/.github/workflows/engine.yml@refs/heads/main"
)

// preservedAnswersFixture is a resumed run as the runner sees it at the
// terminal: the runtime configuration (the example consumer configuration,
// which names knowledge/library/answers, and a writable knowledge root) and
// the sealed envelope the run was claimed with, carrying one adopted answer.
func preservedAnswersFixture(t *testing.T) (runtime.Config, hook.DispatchEnvelope) {
	t.Helper()
	base, err := os.ReadFile("../../config/m1-consumer.json")
	if err != nil {
		t.Fatalf("example consumer config not readable: %v", err)
	}
	consumerPath := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(consumerPath, base, 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := worker.LoadConfig(consumerPath)
	if err != nil {
		t.Fatalf("example consumer config does not load: %v", err)
	}
	if config.AnswerKnowledge == nil {
		t.Fatal("the example consumer config must name an answer knowledge destination")
	}
	snapshot := hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example", ActivityID: 1, ActivityType: 1, ProjectID: 909057, ProjectKey: "TICKET",
		IssueID: 2, IssueKey: "TICKET-3", IssueKeyID: 3, CreatorID: 9903853,
		RunID: "run_20260802_alpha", CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		Target:    hook.DeliveryTarget{RepositoryID: 101, WorkflowRefSHA256: strings.Repeat("a", 64)},
		Untrusted: hook.UntrustedTicketData{Summary: "Change one visible label", Description: "Please reword the visible label on the settings page.\n"},
	}
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	questionsJSON := `[{"id":"Q1","dimension":"preapproved_scope_choice","question":"Which occurrences of the wording should change?","why_blocking":"The visible result differs.","choices":[{"id":"a","label":"Only the heading","effect":"The table keeps the old wording."},{"id":"b","label":"Both occurrences","effect":"Heading and table change together."}]}]`
	question := hook.QuestionRecord{
		Protocol: hook.QuestionProtocolVersion, DeliveryID: envelope.DeliveryID, InputSHA256: envelope.Snapshot.InputSHA256,
		RepositoryID: 42, RepositorySHA256: hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/m1-worker.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40), WorkflowRunID: 123456789, RunAttempt: 1, AutomationRunID: envelope.Snapshot.RunID,
		RunURL:           "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision: 1, QuestionsJSON: questionsJSON, QuestionsSHA256: hook.TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256: strings.Repeat("c", 64), AnswerDeadlineAt: 4_000, NotifyAt: [3]int64{1_000, 2_000, 3_000},
	}
	encodedQuestion, err := hook.MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("fixture question does not seal: %v", err)
	}
	answers := `{"Q1":"b"}`
	record := hook.ClarificationRecord{
		Protocol: hook.ClarificationProtocolVersion, DeliveryID: question.DeliveryID, InputSHA256: question.InputSHA256,
		RepositoryID: question.RepositoryID, RepositorySHA256: question.RepositorySHA256, WorkflowRefSHA256: question.WorkflowRefSHA256,
		AutomationRunID: question.AutomationRunID, InputRevision: 2,
		Rounds: []hook.ClarificationRound{{
			QuestionRecordJSON: string(encodedQuestion), QuestionRecordSHA256: hook.TerminalReportDigest(encodedQuestion),
			QuestionCommentID: 500, AnswerCommentID: 600, AnswererID: 7, AnswerPostedAt: 3_500,
			AnswerBodySHA256: strings.Repeat("b", 64), AnswersJSON: answers, AnswersSHA256: hook.TerminalReportDigest([]byte(answers)),
		}},
	}
	sealed, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("fixture clarification does not seal: %v", err)
	}
	envelope.ClarificationJSON = string(sealed)
	runtimeConfig := runtime.Config{ConsumerConfigPath: consumerPath, KnowledgeRoot: t.TempDir()}
	runtimeConfig.Identity = runtime.IdentityConfig{RepositoryID: 7, Repository: answersTestRepository, WorkflowRef: answersTestWorkflowRef, EngineSHA: strings.Repeat("c", 40)}
	return runtimeConfig, envelope
}

// A resumed run's adopted answers are rendered from the sealed envelope
// alone and written where the reception reads them; the same record twice
// is left alone, a changed one is replaced.
func TestPreservedAnswersAreWrittenIntoTheKnowledgeTree(t *testing.T) {
	config, envelope := preservedAnswersFixture(t)
	artifact, err := renderPreservedAnswers(config, envelope)
	if err != nil || !artifact.Enabled || artifact.IssueKey != "TICKET-3" {
		t.Fatalf("renderPreservedAnswers() = %+v, %v", artifact, err)
	}
	path, written, err := writePreservedAnswers(config.KnowledgeRoot, artifact)
	if err != nil || !written {
		t.Fatalf("first write: %q, %v, %v", path, written, err)
	}
	want := filepath.Join(config.KnowledgeRoot, "knowledge", "library", "answers", "TICKET-3.md")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "回答: b") || !strings.Contains(string(content), "TICKET-3") {
		t.Fatalf("record = %q, %v", content, err)
	}
	workerConfig, err := worker.LoadConfig(config.ConsumerConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, dropped, err := worker.LoadPreservedAnswers(config.KnowledgeRoot, workerConfig)
	if err != nil || dropped != 0 || len(loaded) != 1 || loaded[0].Name != "TICKET-3.md" {
		t.Fatalf("the reception does not read the record back: %+v, %d, %v", loaded, dropped, err)
	}
	if _, written, err := writePreservedAnswers(config.KnowledgeRoot, artifact); err != nil || written {
		t.Fatalf("an identical record was rewritten: %v, %v", written, err)
	}
	changed := artifact
	changed.Content = append(append([]byte(nil), artifact.Content...), "\n- 追記\n"...)
	if _, written, err := writePreservedAnswers(config.KnowledgeRoot, changed); err != nil || !written {
		t.Fatalf("a changed record was not replaced: %v, %v", written, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temporary files were left behind: %d entries", len(entries))
	}
}

// A run that was never resumed has nothing to preserve; a record is never
// written outside the knowledge tree or through a symbolic link.
func TestPreservedAnswersSkipUnresumedRunsAndRefuseEscapesAndSymlinks(t *testing.T) {
	config, envelope := preservedAnswersFixture(t)
	unresumed := envelope
	unresumed.ClarificationJSON = ""
	if artifact, err := renderPreservedAnswers(config, unresumed); err != nil || artifact.Enabled {
		t.Fatalf("an unresumed run rendered a record: %+v, %v", artifact, err)
	}
	artifact, err := renderPreservedAnswers(config, envelope)
	if err != nil {
		t.Fatal(err)
	}
	escaping := artifact
	escaping.Path = "../outside.md"
	if _, _, err := writePreservedAnswers(config.KnowledgeRoot, escaping); err == nil {
		t.Fatal("a path leaving the knowledge tree was written")
	}
	if _, _, err := writePreservedAnswers("", artifact); err == nil {
		t.Fatal("an empty knowledge root was accepted")
	}
	// The answers directory is a symlink to somewhere else: refused.
	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(config.KnowledgeRoot, "knowledge", "library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(config.KnowledgeRoot, "knowledge", "library", "answers")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writePreservedAnswers(config.KnowledgeRoot, artifact); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked answers directory was followed: %v", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("the record landed behind the symlink: %d entries", len(entries))
	}
	// The record itself is a symlink: refused too.
	if err := os.Remove(filepath.Join(config.KnowledgeRoot, "knowledge", "library", "answers")); err != nil {
		t.Fatal(err)
	}
	answersDir := filepath.Join(config.KnowledgeRoot, "knowledge", "library", "answers")
	if err := os.MkdirAll(answersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "record.md"), filepath.Join(answersDir, "TICKET-3.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writePreservedAnswers(config.KnowledgeRoot, artifact); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symlinked record was followed: %v", err)
	}
}

// answersFakeStore acquires and completes every terminal report; the
// ledger side of a run that ends normally.
type answersFakeStore struct{}

func (answersFakeStore) BeginTerminal(context.Context, hook.TerminalBeginRequest) (hook.TerminalBinding, hook.TerminalBeginDisposition, error) {
	return hook.TerminalBinding{IssueID: 2, IssueKey: "TICKET-3"}, hook.TerminalBeginAcquired, nil
}

func (answersFakeStore) CompleteTerminal(context.Context, hook.TerminalCompleteRequest) (hook.TerminalCompleteDisposition, error) {
	return hook.TerminalCompleted, nil
}

type answersFakeComments struct{ posted int }

func (f *answersFakeComments) FindExactComment(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *answersFakeComments) FindCommentWithMarker(context.Context, int64, string) (int64, bool, error) {
	return 0, false, nil
}
func (f *answersFakeComments) AddComment(context.Context, int64, string) (int64, error) {
	f.posted++
	return int64(900 + f.posted), nil
}

// The terminal report is the moment: once it has sealed, the run's adopted
// answers are in the knowledge tree — with nothing in the workspace, as a
// run that died before read-ticket leaves it.
func TestTerminalReportPreservesTheAdoptedAnswers(t *testing.T) {
	config, envelope := preservedAnswersFixture(t)
	route := hook.ReportRouteConfig{
		HMACKey: bytes.Repeat([]byte("k"), 32), RepositoryID: 7,
		RepositorySHA256: hook.HashIdentity(answersTestRepository), WorkflowRefSHA256: hook.HashIdentity(answersTestWorkflowRef),
		ExpectedRunID: envelope.Snapshot.RunID,
		Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
			StagingOrigin: "https://staging.example.test", ProductionOrigin: "https://www.example.test"}},
		ClockSkew: time.Minute, LeaseDuration: time.Minute, SpaceKey: "space", ProjectID: 1, ProjectKey: "TICKET",
		AllowedCreatorID: 1, AllowedActivityType: 1, RunReferenceScheme: "local",
		Target: hook.DeliveryTarget{RepositoryID: 7, WorkflowRefSHA256: hook.HashIdentity(answersTestWorkflowRef)},
	}
	comments := &answersFakeComments{}
	report, err := hook.NewTerminalReportService(route, answersFakeStore{}, comments, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	services := &runtime.Services{Config: config, Report: report, Route: route}
	terminal := NewTerminal(config, services, envelope, 4242, t.TempDir(), trailTestLogger{})
	if err := terminal.Report(context.Background(), hook.TerminalModelFailed, Outcome{Code: hook.TerminalModelFailed}, ""); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	if comments.posted != 1 {
		t.Fatalf("terminal comments = %d, want one", comments.posted)
	}
	record, err := os.ReadFile(filepath.Join(config.KnowledgeRoot, "knowledge", "library", "answers", "TICKET-3.md"))
	if err != nil || !strings.Contains(string(record), "回答: b") {
		t.Fatalf("the record was not preserved at the terminal: %q, %v", record, err)
	}
}
