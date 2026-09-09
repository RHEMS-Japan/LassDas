package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The design reviewer's own instruction has to carry the answers and say
// what they are. Its half of this could be deleted with every test in this
// package green: the runner-side test reads argv from a shell stand-in, and
// nothing asserted the prompt (review of #135).
func TestTheDesignReviewersInstructionCarriesTheDecisions(t *testing.T) {
	decided := "外部公開URLの状態コードを最初に確認する"
	input := designReviewPromptInput{
		lens: "根拠",
		clarification: &worker.ClarificationContext{
			Exchanges: []worker.ClarificationExchange{{
				Questions: []worker.ReadinessQuestion{{ID: "Q1", Question: "最初に確認する項目は？"}},
				Answers:   map[string]string{"Q1": decided},
			}},
		},
	}
	input.subject.Kind = "design"
	prompt, err := designReviewPrompt(input)
	if err != nil {
		t.Fatalf("designReviewPrompt() error = %v", err)
	}
	if !strings.Contains(prompt, decided) {
		t.Fatalf("the reviewer is not given what the requester decided:\n%s", prompt)
	}
	// And is told what it is. Without this the key sits inside a blob the
	// same instruction calls data it must not take instructions from, and
	// the reviewer has no ground to refuse a design that contradicts it.
	for _, rule := range []string{
		"resolved_clarification",
		"依頼者が質問に答えて決めた事項",
		"食い違っていれば",
	} {
		if !strings.Contains(prompt, rule) {
			t.Errorf("the instruction never says what the decisions are (%q)", rule)
		}
	}
	// A run that was never asked anything carries no empty section.
	input.clarification = nil
	plain, err := designReviewPrompt(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain, decided) {
		t.Fatal("a run with no answers carries an answer anyway")
	}
}

// The designer's verb has to wire the flag to the prompt. Both ends were
// held and the middle was not: the verb could read the file and drop it on
// the floor, or lose the flag declaration entirely — which hard-fails every
// resumed run, because an undeclared flag is an argument error — with the
// whole suite green (review of #135).
func TestTheInvestigateVerbAcceptsTheDecisionsFlag(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "worker")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the worker: %v\n%s", err, out)
	}
	// A run with the flag must not be refused for the flag's sake. The verb
	// fails later for want of a real config; what matters is that it does
	// not fail as an argument error.
	missing := filepath.Join(t.TempDir(), "clarification.json")
	if err := os.WriteFile(missing, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "investigate",
		"--config", filepath.Join(t.TempDir(), "consumer.json"),
		"--tool-sha", strings.Repeat("a", 40),
		"--draft", filepath.Join(t.TempDir(), "draft.json"),
		"--repo-root", t.TempDir(), "--base-sha", strings.Repeat("b", 40),
		"--round", "1", "--mode", "design",
		"--measurements", filepath.Join(t.TempDir(), "m.jsonl"),
		"--out-dir", t.TempDir(),
		"--clarification", missing,
	)
	output, _ := command.CombinedOutput()
	if strings.Contains(string(output), "investigate arguments are invalid") {
		t.Fatalf("the verb refuses the flag its caller always passes:\n%s", output)
	}
	if strings.Contains(string(output), "flag provided but not defined") {
		t.Fatalf("the flag is not declared, so every resumed run's design card fails:\n%s", output)
	}
}

// The answers are never shrunk to fit, so a large clarification pushes the
// evidence out. That order is deliberate — the reviewer can read every
// measurement in full from the file on disk, and cannot read the answers
// anywhere — but the head must stop claiming excerpts it did not send.
func TestTheHeadStopsPromisingExcerptsTheBudgetDropped(t *testing.T) {
	present := evidenceNote(evidenceStats{complete: 2})
	if !strings.Contains(present, "引用されていない実測は先頭 2 KiB の抜粋だけです") {
		t.Fatalf("with nothing dropped the head no longer says what it sent:\n%s", present)
	}
	dropped := evidenceNote(evidenceStats{complete: 2, uncitedWithdrawn: 18})
	if strings.Contains(dropped, "引用されていない実測は先頭 2 KiB の抜粋だけです") {
		t.Fatalf("the head promises excerpts it dropped:\n%s", dropped)
	}
	for _, want := range []string{"18 件", "抜粋を渡していません", "measurements.jsonl を読めば"} {
		if !strings.Contains(dropped, want) {
			t.Errorf("the head does not say what was dropped (%q):\n%s", want, dropped)
		}
	}
}
