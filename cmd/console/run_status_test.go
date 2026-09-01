package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

func TestRunStatusSelectsTheFirstLivingStageOfTheNewestCycle(t *testing.T) {
	cards := []runtime.BoardTask{
		{Status: "archived", IdempotencyKey: runtime.ChainCardKey("delivery-a", runtime.StageImplement, 1)},
		{Status: "done", IdempotencyKey: runtime.ChainCardKey("delivery-a", runtime.StageImplement, 2)},
		{Status: "pending", IdempotencyKey: runtime.ChainCardKey("delivery-a", runtime.StageReviewB, 2)},
		{Status: "pending", IdempotencyKey: runtime.ChainCardKey("delivery-a", runtime.StageReviewA, 2)},
		{Status: "running", IdempotencyKey: runtime.ChainCardKey("other", runtime.StageImplement, 9)},
	}
	stage, cycle := selectBoardStage(cards, "delivery-a")
	if stage != runtime.StageReviewA || cycle != 2 {
		t.Fatalf("stage/cycle = %s/%d", stage, cycle)
	}
}

func TestLocalRunProjectionPreservesTheExistingAnswerGate(t *testing.T) {
	row := localRunRow(state.RunOverview{
		Key: "run#delivery-a", IssueKey: "TASK-624", State: "awaiting_answer",
		EnvelopeJSON: `{"snapshot":{"creator_id":1111}}`, QuestionCommentID: 796000001,
		QuestionRecordJSON: `{"answer_deadline_at":4000}`,
	})
	context := answerContextFromRows([]map[string]string{row}, "TASK-624")
	if context.AnswererID != 1111 || context.QuestionCommentID != 796000001 || context.AnswerDeadlineAt != 4000 {
		t.Fatalf("answer context = %+v", context)
	}
}

func TestConsoleSourceBindsTheFourRunStatusLines(t *testing.T) {
	raw, err := os.ReadFile("web/src/App.tsx")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, binding := range []string{"今: {detail.run_status.now}", "ここまで: {detail.run_status.so_far}", "この後: {detail.run_status.next}", "見込み: {detail.run_status.estimate}"} {
		if !strings.Contains(source, binding) {
			t.Fatalf("missing production binding %q", binding)
		}
	}
}

func TestTerminalCTAComesOnlyFromTheSealedOutcomeAndGitHub(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, runner.ChainOutcomeFile)
	if err := os.WriteFile(path, []byte(`{"stage":2,"evidence":{"pull_request_url":"https://github.com/example/repo/pull/7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sealedTerminalURL(path, 2, "example/repo"); got != "https://github.com/example/repo/pull/7" {
		t.Fatalf("url = %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"stage":2,"evidence":{"pull_request_url":"https://evil.example/pull/7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sealedTerminalURL(path, 2, "example/repo"); got != "" {
		t.Fatalf("untrusted url = %q", got)
	}
	if err := os.WriteFile(path, []byte(`{"stage":1,"evidence":{"pull_request_url":"https://github.com/example/repo/pull/7"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sealedTerminalURL(path, 2, "example/repo"); got != "" {
		t.Fatalf("wrong-stage url = %q", got)
	}
}
