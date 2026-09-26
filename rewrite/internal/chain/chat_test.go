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
		content := messages[1].(map[string]any)["content"].(string)
		var state State
		if err := json.Unmarshal([]byte(content), &state); err != nil {
			t.Error(err)
		}
		if len(state.History) != 1 || state.History[0].Output != report {
			t.Error("report was rewritten")
		}
		fmt.Fprint(w, `{"extra":"ignored","choices":[{"message":{"content":"I will hand the work on.","tool_calls":[{"function":{"name":"handoff","arguments":"{\"role\":\"deliver\",\"instruction\":\"Use the corrected build, then run it.\",\"unknown\":true}"}}]}}]}`)
	}))
	defer server.Close()
	roles := map[string]string{"deliver": "publish the reviewed work"}
	primary := DecisionRouter{Judge: Jev{URL: server.URL + "/decisions", Model: "decision-fixture", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}, Roles: roles}
	secondary := ChatRouter{Service: Jev{URL: server.URL + "/chat", Model: "chat-fixture", KeyEnv: "ROUTER_TEST_TOKEN", Client: server.Client()}, Roles: roles}
	result, err := (Alternate{Primary: primary, Secondary: secondary}).Next(context.Background(), State{Request: "original", History: []Result{{Output: report}}})
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
