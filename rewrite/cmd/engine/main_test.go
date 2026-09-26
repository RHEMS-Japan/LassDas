package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This exercises the executable's wiring with API fixtures and a real process.
// It does not establish that a model can finish a real request or deliver it.
func TestIssueReachesProcessUnchangedAndResumesWithoutRepeatingWork(t *testing.T) {
	t.Setenv("TEST_TRACKER_KEY", "synthetic-source-key")
	t.Setenv("TEST_MODEL_KEY", "synthetic-model-key")
	dir := t.TempDir()
	artifact := filepath.Join(dir, "received.txt")
	const original = "Keep every original condition.\nExample: {\"gaps\":null,\"new_field\":true}\nLiteral `words` and $(not-a-command), 日本語."
	wantRequest := "Original issue: EXAMPLE-1\nTitle: Original title\n\n" + original
	modelCalls, trackerCalls := 0, 0
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body any
		switch r.URL.Host {
		case "tracker.example":
			trackerCalls++
			if r.URL.Path != "/api/v2/issues/EXAMPLE-1" || r.URL.Query().Get("apiKey") != "synthetic-source-key" {
				t.Error("wrong issue request")
			}
			body = map[string]any{"issueKey": "EXAMPLE-1", "summary": "Original title", "description": original, "unrecognized": true}
		case "model.example":
			modelCalls++
			if r.Header.Get("Authorization") != "Bearer synthetic-model-key" {
				t.Error("wrong model credential")
			}
			var input struct{ Messages []struct{ Content string } }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || len(input.Messages) != 2 {
				return nil, fmt.Errorf("unexpected routing input: %v", err)
			}
			var state chain.State
			if err := json.Unmarshal([]byte(input.Messages[1].Content), &state); err != nil || state.Request != wantRequest {
				t.Errorf("original request was altered: %q (%v)", state.Request, err)
			}
			assignment := chain.Assignment{Role: "implement", Instruction: "Read the original request; keep its wording."}
			if modelCalls == 2 {
				if len(state.History) != 1 || state.History[0].Output != "Ordinary prose, not a model contract.\n" {
					t.Errorf("unexpected result: %#v", state.History)
				}
				assignment.Role = "done"
			} else if modelCalls > 2 {
				return nil, fmt.Errorf("finished work was repeated")
			}
			args, _ := json.Marshal(assignment)
			body = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "handoff", "arguments": string(args)}}}}}}}
		default:
			return nil, fmt.Errorf("unexpected destination: %s", r.URL.Host)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header), Request: r}, nil
	})
	var cfg config
	cfg.Router.Mode = "llm"
	cfg.Router.LLM = chain.Jev{URL: "https://model.example/chat/completions", Model: "fixture-model", KeyEnv: "TEST_MODEL_KEY"}
	cfg.Backlog = tracker.Backlog{BaseURL: "https://tracker.example/api/v2", KeyEnv: "TEST_TRACKER_KEY"}
	cfg.Roles = []chain.Role{{Name: "implement", Purpose: "Work on the original request.", Processes: []chain.Process{{
		Name: "worker", Directory: dir,
		Command: []string{"/bin/sh", "-c", "cat > \"$1\"; printf '%s\\n' 'Ordinary prose, not a model contract.'", "worker", artifact},
	}}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--config", configPath, "--issue", "EXAMPLE-1", "--run-dir", filepath.Join(dir, "run")}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var log bytes.Buffer
	if err := run(ctx, args, &log); err != nil {
		t.Fatal(err)
	}
	received, err := os.ReadFile(artifact)
	if err != nil || !strings.Contains(string(received), wantRequest) || !strings.Contains(string(received), "Read the original request; keep its wording.") {
		t.Fatalf("process lost the request or assignment: %s (%v)", received, err)
	}
	if err := run(ctx, args, &log); err != nil {
		t.Fatal(err)
	}
	if modelCalls != 2 || trackerCalls != 2 {
		t.Fatalf("routing calls=%d tracker calls=%d", modelCalls, trackerCalls)
	}
	store, err := chain.Open(filepath.Join(dir, "run"), wantRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil || !state.Done || len(state.History) != 1 || state.Pending != nil {
		t.Fatalf("state=%#v error=%v", state, err)
	}
}
