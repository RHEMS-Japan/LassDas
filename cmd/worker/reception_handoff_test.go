package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

type unreadableReceptionAPI struct{ calls int }

func (a *unreadableReceptionAPI) ChatCompletions(context.Context, worker.ModelEndpoint, worker.ChatRequest) (*worker.ChatResponse, error) {
	a.calls++
	return &worker.ChatResponse{
		ID: "unreadable-turn",
		Choices: []worker.ChatChoice{{FinishReason: worker.ChatFinishStop, Message: worker.ChatMessage{
			Role: "assistant", Content: "I cannot produce the requested answer.",
		}}},
		Usage: &worker.ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

// The fallback's contract is not just a valid artifact: its original request
// must reach the actual implementing process and yield a sealable change.
// The model is a stand-in, as is the author; all contract, instruction, launch
// and candidate-sealing code is the production path.
func TestUnreadableReceptionHandsTheOriginalRequestToTheImplementingProcess(t *testing.T) {
	const request = "Update the visible label. Keep every other behavior unchanged."
	fixture := newAgentFixture(t, `case "$*" in *'Keep every other behavior unchanged.'*) ;; *) exit 9 ;; esac; `+editTheLabel, "true")
	raw, err := worker.ReadRawTicket(cliTestEnvelopeWithDescription(t, request), fixture.config, cliToolSHA)
	if err != nil {
		t.Fatal(err)
	}
	api := &unreadableReceptionAPI{}
	invoker, err := worker.NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	intake, _, err := invoker.ReadContract(t.Context(), raw, fixture.config)
	if err != nil || !intake.Fallback || api.calls != 3 {
		t.Fatalf("intake = %+v, calls = %d, error = %v", intake, api.calls, err)
	}
	draft, err := intake.ToDraft(raw, fixture.config)
	if err != nil || draft.Request != request {
		t.Fatalf("draft = %+v, %v", draft, err)
	}
	writeTestJSON(t, fixture.draftPath, draft)
	instruction := fixture.path("INSTRUCTION.md")
	if err := run(t.Context(), []string{
		"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--repo-root", fixture.repoRoot, "--out", instruction,
	}); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(instruction)
	if err != nil || !strings.Contains(string(text), request) {
		t.Fatalf("original request did not reach the instruction: %s, %v", text, err)
	}
	if err := run(t.Context(), []string{
		"run-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--role", "implementer", "--instruction", instruction,
		"--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--stage", "1",
		"--out", fixture.path("implementer-run.json"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t); err != nil {
		t.Fatal(err)
	}
	var candidate worker.Candidate
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)
	if len(candidate.Files) != 1 || !strings.Contains(candidate.Files[0].Content, "Updated label") {
		t.Fatalf("no completed change after the fallback: %+v", candidate.Files)
	}
}
