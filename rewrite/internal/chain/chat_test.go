package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecisionTransportFailureUsesChatInstructionsWithoutLosingReports(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	const report = "There is no prescribed report format. Example {\"extra\":true}. Still need to upload the corrected artifact."
	const diagnostic = "Earlier upload attempt returned no receipt. Later work may have recovered; inspect actual delivery."
	var paths []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer synthetic-router-token" {
			t.Error("credential missing")
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if r.URL.Path == "/decisions" {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":"max_tokens_exceeded"}`)
			return
		}
		messages := request["messages"].([]any)
		policy := messages[0].(map[string]any)["content"].(string)
		if !strings.Contains(policy, "not an earlier implementation") || !strings.Contains(policy, "not independent reviews of the changed work") {
			t.Error("the current-work review instruction did not reach the chat router")
		}
		content := messages[1].(map[string]any)["content"].(string)
		var state State
		if err := json.Unmarshal([]byte(content), &state); err != nil {
			t.Error(err)
		}
		if len(state.History) != 1 || state.History[0].Output != report || state.History[0].Diagnostics != diagnostic {
			t.Error("report or diagnostic was lost")
		}
		fmt.Fprint(w, `{"extra":"ignored","choices":[{"message":{"content":"I will hand the work on.","tool_calls":[{"function":{"name":"handoff","arguments":"{\"role\":\"deliver\",\"instruction\":\"Use the corrected build, then run it.\",\"unknown\":true}"}}]}}]}`)
	}))
	defer server.Close()
	roles := map[string]string{"deliver": "publish the reviewed work"}
	primary := DecisionRouter{Judge: Jev{URL: server.URL + "/decisions", Model: "decision-fixture", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}, Roles: roles}
	secondary := ChatRouter{Service: Jev{URL: server.URL + "/chat", Model: "chat-fixture", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}, Roles: roles}
	result, err := (Alternate{Primary: primary, Secondary: secondary}).Next(context.Background(), State{Request: "original", History: []Result{{Output: report, Diagnostics: diagnostic}}})
	if err != nil || result.Role != "deliver" || result.Instruction != "Use the corrected build, then run it." {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if strings.Join(paths, ",") != "/decisions,/chat" {
		t.Fatalf("requests=%v", paths)
	}
}

func TestModelErrorRedactsCredentialAndNeverFollowsRedirect(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	for _, status := range []int{http.StatusFound, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/other")
				w.WriteHeader(status)
				fmt.Fprint(w, "upstream failed: synthetic-router-token")
			}))
			defer server.Close()
			_, err := (Jev{URL: server.URL, Model: "fixture", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}).Choose(context.Background(), State{}, "choose", map[string]string{"done": "done"})
			if err == nil || strings.Contains(err.Error(), "synthetic-router-token") || !strings.Contains(err.Error(), "upstream failed") {
				t.Fatalf("err=%v", err)
			}
			if calls != 1 {
				t.Fatalf("redirect followed: %d requests", calls)
			}
		})
	}
}

func TestChatJudgeOnlyChoosesListedEndpointsAndCannotFinishWork(t *testing.T) {
	t.Setenv("ROUTER_TEST_TOKEN", "synthetic-router-token")
	for _, answer := range []string{"maker/current", "done", "maker/unknown"} {
		t.Run(answer, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []struct{ Content string }
					Tools    []struct {
						Function struct {
							Parameters struct {
								Properties map[string]struct{ Enum []string }
							}
						}
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				policy := request.Messages[0].Content
				if !strings.Contains(policy, "current model policy") || !strings.Contains(policy, "fresh metadata") || strings.Contains(policy, "Have the configured independent reviewers") {
					t.Errorf("model selection got a work-completion policy: %s", policy)
				}
				if got := request.Tools[0].Function.Parameters.Properties["role"].Enum; len(got) != 1 || got[0] != "maker/current" {
					t.Errorf("extra action made available: %v", got)
				}
				var state State
				if err := json.Unmarshal([]byte(request.Messages[1].Content), &state); err != nil || state.Request != "original 日本語" {
					t.Errorf("original changed: %#v %v", state, err)
				}
				args, _ := json.Marshal(map[string]any{"role": answer, "extra": true})
				json.NewEncoder(w).Encode(map[string]any{"unknown": true, "choices": []any{map[string]any{"message": map[string]any{"content": "Ordinary introductory words.", "tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": string(args)}}}}}}})
			}))
			defer server.Close()
			judge := ChatJudge{Service: Jev{URL: server.URL, Model: "chat-selector", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}}
			selected, err := judge.Choose(context.Background(), State{Request: "original 日本語"}, "current model policy", map[string]string{"maker/current": "fresh metadata"})
			if answer == "maker/current" {
				if err != nil || selected != answer {
					t.Fatalf("selected=%q error=%v", selected, err)
				}
			} else if err == nil || selected != "" {
				t.Fatalf("unconfigured/finish action accepted: %q %v", selected, err)
			}
		})
	}
}
