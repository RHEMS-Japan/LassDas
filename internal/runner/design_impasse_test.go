package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/runtime"
)

// The question a design that would not converge puts to its requester is
// written from the newest sealed round, and the caller learns whether there
// is one to post: a run whose design rounds ran out used to end without a
// word (live 2026-09-17).
func TestAskDesignImpasseWritesTheQuestionForTheNewestRound(t *testing.T) {
	record := filepath.Join(t.TempDir(), "argv.txt")
	workspace := t.TempDir()
	decision := filepath.Join(workspace, "history", "question", "decision.json")
	script := filepath.Join(t.TempDir(), "worker")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + record + "\nmkdir -p " + filepath.Dir(decision) +
		"\nprintf '%s' '{\"outcome\":\"clarification_required\",\"questions\":[{\"id\":\"Q1\"}]}' > " + decision + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: filepath.Join(workspace, "consumer.json")}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}

	// No sealed design: nothing to ask about, and the run ends as it did.
	asked, err := pipeline.AskDesignImpasse(context.Background(), []string{"review-a", "review-b"})
	if err != nil || asked {
		t.Fatalf("a run with no design asked anyway: %v %v", asked, err)
	}

	for round := 1; round <= 2; round++ {
		dir := filepath.Join(workspace, "history", "design-"+string(rune('0'+round)))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"investigation.json", "design.json", "decision.json", "review-a-design-review.json", "review-b-design-review.json"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	asked, err = pipeline.AskDesignImpasse(context.Background(), []string{"review-a", "review-b"})
	if err != nil || !asked {
		t.Fatalf("the question was not written: %v %v", asked, err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"design-impasse-question",
		"--design " + filepath.Join(workspace, "history", "design-2", "design.json"),
		"--review " + filepath.Join(workspace, "history", "design-2", "review-a-design-review.json"),
		"--ticket " + filepath.Join(workspace, "readiness-ticket.json"),
	} {
		if !strings.Contains(string(argv), want) {
			t.Fatalf("argv lacks %q: %s", want, argv)
		}
	}
	if !strings.Contains(string(argv), "review-b-design-review.json") {
		t.Fatalf("a sealed review was left out: %s", argv)
	}

	// A decision that asks nothing leaves the run ending as before.
	if err := os.WriteFile(decision, []byte(`{"outcome":"question_rounds_exhausted","questions":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	asked, err = pipeline.AskDesignImpasse(context.Background(), []string{"review-a"})
	if err != nil || asked {
		t.Fatalf("a spent decision was posted as a question: %v %v", asked, err)
	}
}

// Half a round's reviews is half the disagreement: a question built on one
// of two judges would put the wrong choice to the requester.
func TestAskDesignImpasseWaitsForEveryReview(t *testing.T) {
	workspace := t.TempDir()
	script := filepath.Join(t.TempDir(), "worker")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{WorkerBin: script, ConsumerConfigPath: filepath.Join(workspace, "consumer.json")}
	config.Identity.EngineSHA = strings.Repeat("ab", 20)
	pipeline := &Pipeline{Config: config, Workspace: workspace, Logger: trailTestLogger{}}
	dir := filepath.Join(workspace, "history", "design-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"investigation.json", "design.json", "decision.json", "review-a-design-review.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	asked, err := pipeline.AskDesignImpasse(context.Background(), []string{"review-a", "review-b"})
	if err != nil || asked {
		t.Fatalf("a half-sealed round was asked about: %v %v", asked, err)
	}
}
