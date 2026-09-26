package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

type arbitrationHistoryTransport func(*http.Request) (*http.Response, error)

func (f arbitrationHistoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the production CLI, gateway serialization and sealed record, not
// just the library parameter. The transport never makes a network connection.
func TestArbitrationCLIHandsEarlierAttemptsToTheModel(t *testing.T) {
	arbitrationCLIHistory(t, false)
}

func TestArbitrationCLIReadsRepositoryFactsBeforeRuling(t *testing.T) {
	arbitrationCLIHistory(t, true)
}

func arbitrationCLIHistory(t *testing.T, repository bool) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	writeTestJSON(t, configPath, cliTestConfig())
	config, err := worker.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	request, err := worker.ParseTicketWithToolSHA(cliTestEnvelope(t), config, cliToolSHA)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "repo")
	file := filepath.Join(root, request.TargetFiles[0])
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loader.go"), []byte("package loader\n// A standalone extension exposes GetName and Init without editing the registry.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runCLITestGit(t, root, "init", "-q")
	runCLITestGit(t, root, "add", ".")
	runCLITestGit(t, root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture")
	base := strings.TrimSpace(runCLITestGit(t, root, "rev-parse", "HEAD"))
	source, err := worker.ReadSourceSnapshot(root, base, request, config)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	usage := func(endpoint worker.ModelEndpoint) worker.InvocationUsage {
		return worker.InvocationUsage{RequestedModel: endpoint.Model, RequestID: "fixture-" + endpoint.ID, StopReason: worker.ChatFinishStop, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	}
	historyDir := filepath.Join(dir, "history")
	var current worker.Candidate
	var currentReviews []worker.Review
	for round := 1; round <= 2; round++ {
		stage := filepath.Join(historyDir, fmt.Sprintf("stage-%d", round))
		if err := os.MkdirAll(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		candidate, err := worker.NewCandidate(round, worker.ModelCandidateOutput{Files: []worker.ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}}, Rationale: fmt.Sprintf("Attempt %d retained the same problem.", round)}, source, request, config, usage(config.Models.Implementer), when)
		if err != nil {
			t.Fatal(err)
		}
		var reviews []worker.Review
		for i, seat := range config.Models.Reviewers {
			answer := worker.ModelReviewOutput{Verdict: "pass", Findings: []worker.ModelFinding{}}
			if i == 1 {
				answer = worker.ModelReviewOutput{Verdict: "revise", Findings: []worker.ModelFinding{{Code: "missing-behavior", Path: request.TargetFiles[0], Message: "The requested behavior is still missing."}}}
			}
			review, err := worker.NewReview(round, seat, answer, candidate, source, request, config, usage(seat), when)
			if err != nil {
				t.Fatal(err)
			}
			reviews = append(reviews, review)
			writeTestJSON(t, filepath.Join(stage, seat.ID+".json"), review)
		}
		decision, err := worker.DecideStage(candidate, reviews, source, request, config, nil)
		if err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]any{"ticket.json": request, "source.json": source, "candidate.json": candidate, "decision.json": decision} {
			writeTestJSON(t, filepath.Join(stage, name), value)
		}
		current, currentReviews = candidate, reviews
	}
	t.Setenv(config.Models.ArbiterEndpoint().APIKeyEnv, "fixture-only-no-network")
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	calls := 0
	http.DefaultTransport = arbitrationHistoryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var turn worker.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&turn); err != nil {
			return nil, err
		}
		var prompt struct {
			History *worker.ArbitrationHistory `json:"previous_attempts"`
		}
		if err := json.Unmarshal([]byte(turn.Messages[1].Content), &prompt); err != nil {
			return nil, err
		}
		if prompt.History == nil || len(prompt.History.Rounds) != 1 || prompt.History.Rounds[0].Round != 1 || prompt.History.Rounds[0].Rationale != "Attempt 1 retained the same problem." {
			t.Error("the CLI did not pass the first attempt through the real model request")
		}
		answer := `{"ruling":"instruct_implementer","instruction":"Investigate an in-scope alternative instead of repeating the failed change.","overruled":[],"statement":"The prior attempt did not meet the request.","evidence":"The earlier review."}`
		if repository {
			if calls == 1 {
				answer = `{"probe":{"probe":"repo.read","args":{"path":"loader.go"}}}`
			} else {
				last := turn.Messages[len(turn.Messages)-1].Content
				if !strings.Contains(last, "standalone extension exposes GetName and Init") {
					t.Fatalf("the arbiter was not shown the actual loader: %s", last)
				}
				answer = `{"ruling":"instruct_implementer","instruction":"Implement the standalone extension entry points within the permitted new files, without editing the registry.","overruled":[],"statement":"The existing loader supports standalone extensions.","evidence":"The repository measurement of loader.go."}`
			}
		}
		response := worker.ChatResponse{ID: "fixture-arbitration", Choices: []worker.ChatChoice{{FinishReason: worker.ChatFinishStop, Message: worker.ChatMessage{Role: "assistant", Content: answer}}}, Usage: &worker.ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
		body, err := json.Marshal(response)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body))}, err
	})
	stage := filepath.Join(historyDir, "stage-2")
	args := []string{"arbitrate", "--config", configPath, "--tool-sha", cliToolSHA, "--ticket", filepath.Join(stage, "ticket.json"), "--source", filepath.Join(stage, "source.json"), "--candidate", filepath.Join(stage, "candidate.json"), "--history", historyDir, "--out", filepath.Join(stage, "ruling.json")}
	if repository {
		args = append(args, "--repo-root", root)
	}
	for _, seat := range config.Models.Reviewers {
		args = append(args, "--review", filepath.Join(stage, seat.ID+".json"))
	}
	if err := run(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	wantCalls := 1
	if repository {
		wantCalls = 2
	}
	if calls != wantCalls {
		t.Fatalf("model calls=%d, want %d", calls, wantCalls)
	}
	ruling, err := worker.ReadRulingFile(filepath.Join(stage, "ruling.json"))
	if err != nil || ruling == nil || ruling.History == nil || len(ruling.History.Rounds) != 1 {
		t.Fatalf("no sealed history: %+v %v", ruling, err)
	}
	if err := ruling.Validate(current, currentReviews, request, config); err != nil {
		t.Fatal(err)
	}
	if repository {
		body, _ := json.Marshal(ruling)
		if !bytes.Contains(body, []byte("standalone extension exposes GetName and Init")) {
			t.Fatal("the ruling did not retain what the arbiter actually read")
		}
		if dirty := runCLITestGit(t, root, "status", "--porcelain=v1", "--untracked-files=all"); dirty != "" {
			t.Fatalf("arbitration changed the checkout: %s", dirty)
		}
	}
}
