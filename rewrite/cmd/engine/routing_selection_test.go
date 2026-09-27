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

func routingSelectionReply(request *http.Request, assignment chain.Assignment) *http.Response {
	arguments, _ := json.Marshal(assignment)
	return selectionReply(request, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": string(arguments)}}},
	}}}})
}

func TestSelectedRouterRefreshesAndDoesNotUseEarlierModelOnFailure(t *testing.T) {
	for _, failure := range []string{"catalog", "selector", "unlisted-choice"} {
		t.Run(failure, func(t *testing.T) {
			selector := testSelector(t)
			catalogs, chats := 0, 0
			state := chain.State{Request: "Original request 日本語", History: []chain.Result{
				{Role: "review", Speaker: "a", Output: "Earlier objection, not an approval stamp."},
				{Role: "implement", Speaker: "worker", Output: "Ordinary prose with no schema; a change was made."},
			}}
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai":
					catalogs++
					if request.Header.Get("Authorization") != "" || request.Header.Get("Cache-Control") != "no-cache" {
						t.Error("catalog request used a credential or allowed cache")
					}
					if catalogs == 2 && failure == "catalog" {
						return catalogReply(request, 503, "catalog unavailable"), nil
					}
					return selectionReply(request, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("maker-one/current-%d", catalogs))}}), nil
				case "selection.example":
					if catalogs == 2 && failure == "selector" {
						return catalogReply(request, 503, "selector unavailable"), nil
					}
					choice := fmt.Sprintf("maker-one/current-%d", catalogs)
					if catalogs == 2 && failure == "unlisted-choice" {
						choice = "maker-one/current-1"
					}
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
				case "routing.example":
					chats++
					var input struct {
						Model    string
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						return nil, err
					}
					if catalogs == 2 || input.Model != fmt.Sprintf("maker-one/current-%d", catalogs) {
						t.Errorf("stale selection used: model=%s catalogs=%d", input.Model, catalogs)
					}
					var got chain.State
					if len(input.Messages) != 2 || json.Unmarshal([]byte(input.Messages[1].Content), &got) != nil || !reflect.DeepEqual(got, state) {
						t.Errorf("routing input was rewritten: %+v", got)
					}
					if !strings.Contains(input.Messages[0].Content, "operator responsibility") {
						t.Error("operator instructions lost")
					}
					return routingSelectionReply(request, chain.Assignment{Role: "review", Instruction: "Inspect the changed work, in ordinary prose."}), nil
				default:
					return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
				}
			})
			router := selectedChatRouter{selection: selector, chat: chain.ChatRouter{
				Service: chain.Jev{URL: "https://routing.example/chat", Model: "never-use-pinned", KeyEnv: "SELECTION_TEST_KEY"},
				Roles:   map[string]string{"review": "Independent review"}, Instructions: "operator responsibility",
			}}
			for call := 1; call <= 3; call++ {
				assignment, err := router.Next(context.Background(), state)
				if call == 2 {
					if err == nil || assignment.Role != "" || chats != 1 {
						t.Fatalf("selection failure was hidden: assignment=%+v err=%v chats=%d", assignment, err, chats)
					}
				} else if err != nil || assignment.Role != "review" || assignment.Instruction == "" {
					t.Fatalf("routing failed: %+v %v", assignment, err)
				}
			}
			if catalogs != 3 || chats != 2 || router.chat.Service.Model != "never-use-pinned" {
				t.Fatalf("shared configuration mutated or stale catalog reused: catalogs=%d chats=%d router=%+v", catalogs, chats, router)
			}
		})
	}
}

func TestExecutableSelectsFreshChatRouterIncludingDecisionFallback(t *testing.T) {
	for _, mode := range []string{"llm", "jev"} {
		t.Run(mode, func(t *testing.T) {
			selector := testSelector(t)
			catalogs, primary, chats := 0, 0, 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai":
					catalogs++
					return selectionReply(request, 200, map[string]any{"data": []any{selectionModel(fmt.Sprintf("maker-one/current-%d", catalogs))}}), nil
				case "selection.example":
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": fmt.Sprintf("maker-one/current-%d", catalogs)}}}), nil
				case "decision.example":
					primary++
					return catalogReply(request, 503, "temporary primary outage"), nil
				case "routing.example":
					chats++
					var input struct {
						Model    string
						Messages []struct{ Role, Content string }
					}
					if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
						return nil, err
					}
					if input.Model != fmt.Sprintf("maker-one/current-%d", chats) || catalogs != chats {
						t.Errorf("public run used stale routing model: %s catalogs=%d chats=%d", input.Model, catalogs, chats)
					}
					var state chain.State
					if len(input.Messages) != 2 || json.Unmarshal([]byte(input.Messages[1].Content), &state) != nil || state.Request != "original request" {
						t.Fatal("routing lost original request")
					}
					if chats == 1 {
						return routingSelectionReply(request, chain.Assignment{Role: "implement", Instruction: "Exercise the configured process."}), nil
					}
					if chats != 2 || len(state.History) != 1 || state.History[0].Output != "Unformatted process report.\n" || state.History[0].Instruction != "Exercise the configured process." {
						t.Fatalf("work/handoff not preserved: %+v", state)
					}
					return routingSelectionReply(request, chain.Assignment{Role: "done"}), nil
				default:
					return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
				}
			})
			var cfg config
			cfg.Router.Mode = mode
			cfg.Router.Decision = chain.Jev{URL: "https://decision.example/decisions", Model: "decision-model", KeyEnv: "SELECTION_TEST_KEY"}
			cfg.Router.LLM = chain.Jev{URL: "https://routing.example/chat", KeyEnv: "SELECTION_TEST_KEY"}
			cfg.ModelSelection = &selector
			cfg.Roles = []chain.Role{{Name: "implement", Processes: []chain.Process{{Name: "worker", Command: []string{"/bin/sh", "-c", "printf 'Unformatted process report.\\n'"}}}}}
			dir := t.TempDir()
			configPath, requestPath := filepath.Join(dir, "operator.json"), filepath.Join(dir, "request.txt")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(requestPath, []byte("original request"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var log bytes.Buffer
			if err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(dir, "run")}, io.Discard, &log); err != nil {
				t.Fatal(err)
			}
			if catalogs != 2 || chats != 2 || (mode == "jev" && primary != 2) || (mode == "llm" && primary != 0) || strings.Count(log.String(), "routing with freshly selected model:") != 2 {
				t.Fatalf("selection not connected: catalogs=%d chats=%d primary=%d log=%s", catalogs, chats, primary, &log)
			}
		})
	}
}

func TestRoutingOutageReachesFreshSelectionAndNextRole(t *testing.T) {
	selector := testSelector(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	const requestText = "Deliver the original request, unchanged 日本語."
	store, err := chain.Open(t.TempDir(), requestText)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalogs, selections, chats, failedCalls := 0, 0, 0, 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			catalogs++
			return selectionReply(request, 200, map[string]any{"data": []any{
				selectionModel("maker-one/current"), selectionModel("maker-two/current"),
			}}), nil
		case "selection.example":
			selections++
			var input struct{ State chain.State }
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			if input.State.Request != requestText {
				t.Error("model selection lost the original request")
			}
			// The fixture changes its choice only if the actual failed call
			// reaches it. This tests information flow, not LLM reasoning quality.
			choice := "maker-one/current"
			for _, result := range input.State.History {
				if result.Role == "router" && strings.Contains(result.Error, "maker-one/current") && strings.Contains(result.Error, "HTTP 503") {
					choice = "maker-two/current"
				}
			}
			if data, _ := json.Marshal(input); strings.Contains(string(data), "synthetic-selection-only") {
				t.Error("failure observation exposed a credential")
			}
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		case "routing.example":
			chats++
			var input struct {
				Model    string
				Messages []struct{ Role, Content string }
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			if input.Model == "maker-one/current" {
				failedCalls++
				if failedCalls == 2 {
					cancel() // Bound the broken implementation's uninformative retry.
				}
				return catalogReply(request, 503, "temporarily unavailable; synthetic-selection-only"), nil
			}
			var state chain.State
			if len(input.Messages) != 2 || json.Unmarshal([]byte(input.Messages[1].Content), &state) != nil {
				t.Fatal("cannot observe routing input")
			}
			if state.Request != requestText || len(state.History) == 0 || state.History[0].Role != "router" {
				t.Fatalf("router did not get the original request and outage: %+v", state)
			}
			for _, result := range state.History {
				if strings.HasSuffix(result.Output, "ordinary work report\n") {
					return routingSelectionReply(request, chain.Assignment{Role: "done"}), nil
				}
			}
			return routingSelectionReply(request, chain.Assignment{Role: "implement", Instruction: "Continue the original work."}), nil
		default:
			return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
		}
	})
	engine := chain.Chain{Store: store, RetryDelay: time.Millisecond,
		Router: selectedChatRouter{selection: selector, chat: chain.ChatRouter{
			Service: chain.Jev{URL: "https://routing.example/chat", KeyEnv: "SELECTION_TEST_KEY"},
			Roles:   map[string]string{"implement": "Implement the request"},
		}},
		Executor: chain.Processes{Roles: map[string]chain.Role{"implement": {
			Name: "implement", Processes: []chain.Process{{Name: "worker", Command: []string{"/bin/sh", "-c", "cat; printf 'ordinary work report\\n'"}}},
		}}},
	}
	if err := engine.Run(ctx); err != nil {
		t.Fatalf("recovery could not use the routing failure: %v catalogs=%d selections=%d chats=%d", err, catalogs, selections, chats)
	}
	state, err := store.Load()
	if err != nil || !state.Done || len(state.History) != 2 || failedCalls != 1 || catalogs != 3 || selections != 3 || chats != 3 {
		t.Fatalf("unexpected recovery: state=%+v err=%v catalogs=%d selections=%d chats=%d failures=%d", state, err, catalogs, selections, chats, failedCalls)
	}
	for _, text := range []string{requestText, "maker-one/current", "HTTP 503", "temporarily unavailable"} {
		if !strings.Contains(state.History[1].Output, text) {
			t.Errorf("next process did not receive %q", text)
		}
	}
	if data, _ := json.Marshal(state); strings.Contains(string(data), "synthetic-selection-only") {
		t.Error("persisted failure exposed a credential")
	}
}
