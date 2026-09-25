package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

// How long a requester has to answer is the destination's to set, and the
// question is posted with that window sealed into it. A destination that
// cannot be read is not a reason to leave the question unasked: the standard
// window stands and the requester still sees it.
func TestTheAnswerWindowComesFromTheDestination(t *testing.T) {
	config, err := worker.LoadConfig("../../config/m1-consumer.json")
	if err != nil {
		t.Fatal(err)
	}
	config.QuestionDeadlineWeekdays = 3
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	terminal := &Terminal{config: runtime.Config{ConsumerConfigPath: path}}
	if got := terminal.answerWeekdays(); got != 3 {
		t.Fatalf("answer window = %d weekdays, want the 3 the destination set", got)
	}
	unreadable := &Terminal{config: runtime.Config{ConsumerConfigPath: filepath.Join(t.TempDir(), "absent.json")}}
	if got := unreadable.answerWeekdays(); got != hook.DefaultQuestionDeadlineWeekdays {
		t.Fatalf("answer window = %d weekdays, want the standard %d", got, hook.DefaultQuestionDeadlineWeekdays)
	}
}
