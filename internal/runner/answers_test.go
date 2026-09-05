package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// preservedAnswersFixture lays out a resumed run's workspace: the sealed raw
// ticket and the sealed clarification with one adopted answer, against the
// example consumer configuration (which names knowledge/library/answers).
func preservedAnswersFixture(t *testing.T) (runtime.Config, string) {
	t.Helper()
	base, err := os.ReadFile("../../config/m1-consumer.json")
	if err != nil {
		t.Skip("example consumer config not available")
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
	raw, err := worker.ReadRawTicket(envelope, config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatalf("raw ticket: %v", err)
	}
	questionsJSON := `[{"id":"Q1","dimension":"preapproved_scope_choice","question":"Which occurrences of the wording should change?","why_blocking":"The visible result differs.","choices":[{"id":"a","label":"Only the heading","effect":"The table keeps the old wording."},{"id":"b","label":"Both occurrences","effect":"Heading and table change together."}]}]`
	question := hook.QuestionRecord{
		Protocol: hook.QuestionProtocolVersion, DeliveryID: raw.DeliveryID, InputSHA256: raw.InputSHA256,
		RepositoryID: 42, RepositorySHA256: hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/m1-worker.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40), WorkflowRunID: 123456789, RunAttempt: 1, AutomationRunID: raw.RunID,
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
	workspace := t.TempDir()
	encodedRaw, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(workspace, "raw-ticket.json"), encodedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "clarification.json"), sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	return runtime.Config{ConsumerConfigPath: consumerPath, KnowledgeRoot: t.TempDir()}, workspace
}

// A resumed run's adopted answers are rendered from its sealed artifacts and
// written where the reception reads them; the same record twice is left
// alone, a changed one is replaced.
func TestPreservedAnswersAreWrittenIntoTheKnowledgeTree(t *testing.T) {
	config, workspace := preservedAnswersFixture(t)
	artifact, err := renderPreservedAnswers(config, workspace)
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
	changed.Content = append([]byte(nil), artifact.Content...)
	changed.Content = append(changed.Content, "\n- 追記\n"...)
	if _, written, err := writePreservedAnswers(config.KnowledgeRoot, changed); err != nil || !written {
		t.Fatalf("a changed record was not replaced: %v, %v", written, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temporary files were left behind: %d entries", len(entries))
	}
}

// A run that was never resumed has nothing to preserve, and a record can
// never be written outside the knowledge tree.
func TestPreservedAnswersSkipUnresumedRunsAndStayInsideTheTree(t *testing.T) {
	config, workspace := preservedAnswersFixture(t)
	if err := os.Remove(filepath.Join(workspace, "clarification.json")); err != nil {
		t.Fatal(err)
	}
	artifact, err := renderPreservedAnswers(config, workspace)
	if err != nil || artifact.Enabled {
		t.Fatalf("an unresumed run rendered a record: %+v, %v", artifact, err)
	}
	escaping := worker.AnswerKnowledgeArtifact{Enabled: true, Path: "../outside.md", IssueKey: "TICKET-3", Content: []byte("x")}
	if _, _, err := writePreservedAnswers(config.KnowledgeRoot, escaping); err == nil {
		t.Fatal("a path leaving the knowledge tree was written")
	}
	if _, _, err := writePreservedAnswers("", worker.AnswerKnowledgeArtifact{Enabled: true, Path: "a/b.md", Content: []byte("x")}); err == nil {
		t.Fatal("an empty knowledge root was accepted")
	}
}
