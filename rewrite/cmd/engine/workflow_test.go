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
	"reflect"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func TestRoutingRoleDescriptionUsesOnlyOperatorWorkFacts(t *testing.T) {
	role := chain.Role{Name: "inspect", Purpose: "Inspect the delivered work.", Processes: []chain.Process{
		{Name: "left", Instructions: "Read assigned comments with the provided tool.", TrackerAccess: "read",
			Command: []string{"must-not-reveal-command"}, Directory: "must-not-reveal-directory",
			Env:         map[string]string{"PRIVATE_ENV": "must-not-reveal-env"},
			Secrets:     map[string]string{"KEY": "must-not-reveal-secret-source"},
			Credentials: map[string]string{"OTHER": "must-not-reveal-credential"}},
		{Name: "right", Instructions: "Inspect the archive; no tracker tool."},
	}}
	text := routingRoleDescription(role)
	for _, want := range []string{role.Purpose, "process left; engine-issued tracker access: read",
		"process right; engine-issued tracker access: none", role.Processes[0].Instructions, role.Processes[1].Instructions} {
		if !strings.Contains(text, want) {
			t.Errorf("missing configured work fact %q in %s", want, text)
		}
	}
	if strings.Contains(text, "must-not-reveal") || strings.Contains(text, "PRIVATE_ENV") {
		t.Fatal("non-instruction process data reached the router")
	}
	if role.Processes[0].Instructions != "Read assigned comments with the provided tool." || role.Processes[1].TrackerAccess != "" {
		t.Fatal("describing a role mutated its configuration")
	}
}

// Real child processes, persisted configuration and both native routing API
// protocols. The model choices here are fixtures, not semantic review evidence.
func TestWorkflowConfigurationRunsProcessesAndResumes(t *testing.T) {
	for _, mode := range []string{"jev", "llm"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("TEST_WORKFLOW_KEY", "synthetic-model-key")
			dir := t.TempDir()
			var cfg config
			cfg.Router.Mode = mode
			service := chain.Jev{URL: "https://model.example/route", Model: "fixture", KeyEnv: "TEST_WORKFLOW_KEY"}
			cfg.Router.Decision, cfg.Router.LLM = service, service
			cfg.Workflow = &chain.Workflow{
				Start:   []string{"prepare"},
				After:   map[string][]string{"prepare": {"inspect"}, "repair": {"prepare"}, "inspect": {"publish"}, "publish": {"observe"}, "observe": {"summarize"}, "summarize": {"done"}},
				Recover: map[string][]string{"prepare": {"repair"}, "repair": {"repair"}, "inspect": {"prepare"}, "publish": {"observe"}, "observe": {"repair"}, "summarize": {"observe"}},
			}
			for _, name := range []string{"prepare", "repair", "inspect", "publish", "observe", "summarize"} {
				script := `printf '%s\n' "$1" >> trace; printf '%s\n' 'Free prose {"done":true}; no required output fields.'`
				if name == "prepare" {
					script += `; if [ ! -f repaired ]; then printf '%s\n' 'temporary tool failure' >&2; exit 7; fi`
				}
				if name == "repair" {
					script += `; touch repaired`
				}
				cfg.Roles = append(cfg.Roles, chain.Role{Name: name, Purpose: name, Processes: []chain.Process{{Name: name, Directory: dir,
					Instructions: "Configured work boundary for " + name + ": only its assigned directory.",
					Env:          map[string]string{"PRIVATE_SETTING": "do-not-send-this-environment-value"},
					Command:      []string{"/bin/sh", "-c", script, "role", name}}}})
			}
			want := []string{"prepare", "repair", "prepare", "inspect", "publish", "observe", "summarize", "done"}
			calls := 0
			previous := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = previous })
			http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "model.example" {
					return nil, fmt.Errorf("unexpected destination")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				if calls >= len(want) {
					return nil, fmt.Errorf("completed action repeated")
				}
				choice := want[calls]
				calls++
				// Exercise the actual CLI-to-router wiring, not a standalone
				// prompt helper. Both transports need configured process facts.
				encodedRequest, _ := json.Marshal(body)
				if choice != "done" && !strings.Contains(string(encodedRequest), "Configured work boundary for "+choice) {
					t.Errorf("router lost the configured process instructions for %s", choice)
				}
				if strings.Contains(string(encodedRequest), "do-not-send-this-environment-value") {
					t.Error("router received process environment values")
				}
				var response any
				if mode == "jev" {
					criteria := body["questions"].(map[string]any)["next"].(map[string]any)["criteria"].(map[string]any)
					if len(criteria) != 1 || criteria[choice] == nil {
						t.Errorf("choices=%v want=%s", criteria, choice)
					}
					response = map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}
				} else {
					function := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
					enum := function["parameters"].(map[string]any)["properties"].(map[string]any)["role"].(map[string]any)["enum"].([]any)
					if len(enum) != 1 || enum[0] != choice {
						t.Errorf("choices=%v want=%s", enum, choice)
					}
					args, _ := json.Marshal(chain.Assignment{Role: choice})
					response = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": string(args)}}}}}}}
				}
				encoded, err := json.Marshal(response)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header), Request: r}, err
			})
			configPath, requestPath, runPath := filepath.Join(dir, "config.json"), filepath.Join(dir, "request.txt"), filepath.Join(dir, "run")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			const request = "Keep the full request 日本語, including delivery and reporting."
			if err := os.WriteFile(requestPath, []byte(request), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--config", configPath, "--request", requestPath, "--run-dir", runPath}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var log bytes.Buffer
			if err := run(ctx, args, io.Discard, &log); err != nil {
				t.Fatalf("%v\n%s", err, &log)
			}
			if err := run(ctx, args, io.Discard, &log); err != nil {
				t.Fatal(err)
			}
			trace, err := os.ReadFile(filepath.Join(dir, "trace"))
			if err != nil || !reflect.DeepEqual(strings.Fields(string(trace)), want[:len(want)-1]) || calls != len(want) {
				t.Fatalf("trace=%s calls=%d err=%v", trace, calls, err)
			}
			store, err := chain.Open(runPath, request)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			state, err := store.Load()
			if err != nil || !state.Done || state.Step != "summarize" || state.Recovering || !reflect.DeepEqual(state.Workflow, cfg.Workflow) {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if state.History[0].Error == "" || !strings.Contains(state.History[0].Diagnostics, "temporary tool failure") {
				t.Fatal("lost process failure")
			}
		})
	}
}
