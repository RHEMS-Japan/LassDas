package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
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

// Unlike unreadable-output fallback, this goes through the real decision CLI
// with complete readable assessment/check artifacts. The author must receive
// both the requested work and its explicit restriction before any edit.
func TestInconclusiveReceptionHandsTheOriginalRequestToTheImplementingProcess(t *testing.T) {
	const requestText = "Update the visible label. Keep every other behavior unchanged."
	fixture := newTunedAgentFixture(t, `case "$*" in *'Keep every other behavior unchanged.'*) ;; *) exit 9 ;; esac; `+editTheLabel, "true",
		func(_ string, config *worker.Config) {
			config.Consumers[0].Design = &worker.DesignConfig{Default: worker.DesignDefaultOff}
		})
	var draft worker.TicketDraft
	readAgentArtifact(t, fixture.draftPath, worker.MaxTicketJSONBytes, &draft)
	draft.Request = requestText
	writeTestJSON(t, fixture.draftPath, draft)
	ticketPath, sourcePath := fixture.path("readiness-ticket.json"), fixture.path("readiness-source.json")
	if err := run(t.Context(), []string{"locate-target", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--repo-root", fixture.repoRoot, "--out", ticketPath}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"snapshot", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--ticket", ticketPath, "--repo-root", fixture.repoRoot, "--base-sha", fixture.baseSHA, "--out", sourcePath}); err != nil {
		t.Fatal(err)
	}
	var request worker.TicketRequest
	var source worker.SourceSnapshot
	readAgentArtifact(t, ticketPath, worker.MaxTicketJSONBytes, &request)
	readAgentArtifact(t, sourcePath, worker.MaxArtifactJSONBytes, &source)
	usage := func(endpoint worker.ModelEndpoint) worker.InvocationUsage {
		return worker.InvocationUsage{RequestedModel: endpoint.Model, RequestID: "fixture-" + endpoint.ID,
			StopReason: worker.ChatFinishStop, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	}
	assessment, err := worker.NewReadinessAssessment(1, worker.ModelReadinessOutput{
		Decision: worker.ReadinessAssessorUnresolvable, Questions: []worker.ReadinessQuestion{}, Assumptions: []worker.ReadinessAssumption{},
	}, nil, nil, source, request, fixture.config, usage(fixture.config.Models.Readiness.Assessor), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	check, err := worker.NewReadinessCheck(worker.ModelReadinessCheckOutput{Verdict: "pass", Reasons: []worker.ReadinessCheckReason{}},
		assessment, source, request, fixture.config, usage(fixture.config.Models.Readiness.Checker), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	assessmentPath, checkPath, decisionPath := fixture.path("assessment.json"), fixture.path("check.json"), fixture.path("history/readiness/decision.json")
	writeTestJSON(t, assessmentPath, assessment)
	writeTestJSON(t, checkPath, check)
	if err := os.MkdirAll(fixture.path("history/readiness"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"decide-readiness", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--ticket", ticketPath, "--source", sourcePath, "--assessment", assessmentPath, "--check", checkPath, "--out", decisionPath}); err != nil {
		t.Fatal(err)
	}
	var decision worker.ReadinessDecision
	readAgentArtifact(t, decisionPath, worker.MaxReadinessJSONBytes, &decision)
	if decision.Outcome != worker.ReadinessOutcomeReady || !decision.InconclusiveReading || decision.Fallback ||
		decision.Validate([]worker.ReadinessAssessment{assessment}, []worker.ReadinessCheck{check}, source, request, fixture.config) != nil {
		t.Fatalf("the CLI did not seal an honest accepted decision: %+v", decision)
	}
	if plan, err := runner.ChainPlanFromDecision(fixture.directory, fixture.configPath); err != nil || plan.Shape != runtime.ShapeImplement {
		t.Fatalf("the gate did not reach its implementing card: %+v, %v", plan, err)
	}
	instruction := fixture.path("INSTRUCTION.md")
	if err := run(t.Context(), []string{"implement-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--repo-root", fixture.repoRoot, "--out", instruction}); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(instruction)
	if err != nil || !strings.Contains(string(text), requestText) {
		t.Fatalf("the original request or its constraint was lost: %s, %v", text, err)
	}
	if err := run(t.Context(), []string{"run-instruction", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
		"--draft", fixture.draftPath, "--role", "implementer", "--instruction", instruction, "--repo-root", fixture.repoRoot,
		"--base-sha", fixture.baseSHA, "--stage", "1", "--out", fixture.path("implementer-run.json")}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t); err != nil {
		t.Fatal(err)
	}
	var candidate worker.Candidate
	readAgentArtifact(t, fixture.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)
	if len(candidate.Files) != 1 || !strings.Contains(candidate.Files[0].Content, "Updated label") {
		t.Fatalf("the author produced no bound change: %+v", candidate.Files)
	}
}
