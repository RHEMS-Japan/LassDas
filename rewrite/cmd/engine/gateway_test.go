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
	"sort"
	"strings"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

func gatewaySelector(t *testing.T) selectionConfig {
	t.Helper()
	t.Setenv("SELECTION_TEST_KEY", "synthetic-selection-only")
	t.Setenv("GATEWAY_TEST_KEY", "synthetic-gateway-only")
	return selectionConfig{
		Judge:   chain.Jev{URL: "https://selection.example/decisions", Model: "decision-model", KeyEnv: "SELECTION_TEST_KEY"},
		Authors: []string{"a"},
		Gateway: &gatewayConfig{ModelsURL: "https://gateway.example/v1/models", KeyEnv: "GATEWAY_TEST_KEY", Prefix: "openrouter/"},
	}
}

// The gateway publishes ids, dates and an owner only: no prices, no
// supported_parameters. Eligibility must keep coming from the public catalog.
func gatewayEntry(id string) map[string]any {
	return map[string]any{"id": id, "created": 1234, "object": "model", "owned_by": "gateway"}
}

func offeredChoices(t *testing.T, request *http.Request) []string {
	t.Helper()
	var input struct {
		Questions map[string]struct{ Criteria map[string]string } `json:"questions"`
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		t.Fatal(err)
	}
	var offered []string
	for id := range input.Questions["next"].Criteria {
		offered = append(offered, id)
	}
	sort.Strings(offered)
	return offered
}

// The eligible catalog holds a/x and a/z; the gateway serves only a/x under
// its prefix, plus a model of its own that is not an OpenRouter endpoint.
func TestGatewayOffersOnlyServedModelsAndInvokesThemThroughItsPrefix(t *testing.T) {
	for _, through := range []bool{false, true} {
		t.Run(fmt.Sprintf("gateway=%t", through), func(t *testing.T) {
			selector := gatewaySelector(t)
			if !through {
				selector.Gateway = nil
			}
			var offered []string
			catalogs, lists, selections, routes := 0, 0, 0, 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai":
					catalogs++
					return selectionReply(request, 200, map[string]any{"data": []any{
						selectionModel("a/x"), selectionModel("a/z"),
					}}), nil
				case "gateway.example":
					lists++
					if request.Method != http.MethodGet || request.URL.Path != "/v1/models" {
						t.Errorf("wrong gateway list query: %s %s", request.Method, request.URL)
					}
					if request.Header.Get("Authorization") != "Bearer synthetic-gateway-only" {
						t.Error("gateway list did not use its own named credential")
					}
					return selectionReply(request, 200, map[string]any{"data": []any{
						gatewayEntry("openrouter/a/x"), gatewayEntry("other/y"),
					}}), nil
				case "selection.example":
					selections++
					offered = offeredChoices(t, request)
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "a/x"}}}), nil
				case "routing.example":
					routes++
					choice := "implement"
					if routes == 2 {
						choice = "done"
					} else if routes > 2 {
						return nil, fmt.Errorf("unexpected additional routing")
					}
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
				}
				return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
			})
			var cfg config
			cfg.Router.Mode = "jev"
			cfg.Router.Decision = chain.Jev{URL: "https://routing.example/decisions", Model: "router", KeyEnv: "SELECTION_TEST_KEY"}
			cfg.ModelSelection = &selector
			cfg.Roles = []chain.Role{{Name: "implement", Purpose: "implement", Processes: []chain.Process{{
				Name: "worker", ModelEnv: "CHOSEN_MODEL", Env: map[string]string{"CHOSEN_MODEL": "stale/pinned"},
				Command: []string{"/bin/sh", "-c", `test -z "$GATEWAY_TEST_KEY" || exit 9; printf '%s' "$CHOSEN_MODEL"`},
			}}}}
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var log bytes.Buffer
			runDir := filepath.Join(dir, "run")
			if err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", runDir}, io.Discard, &log); err != nil {
				t.Fatalf("%v\n%s", err, &log)
			}
			wantOffered, wantEndpoint, wantPrefix, wantLists := []string{"a/x", "a/z"}, "a/x", "", 0
			if through {
				wantOffered, wantEndpoint, wantPrefix, wantLists = []string{"a/x"}, "openrouter/a/x", "openrouter/", 1
			}
			if !reflect.DeepEqual(offered, wantOffered) {
				t.Fatalf("judge was offered %v, want %v", offered, wantOffered)
			}
			if catalogs != 1 || lists != wantLists || selections != 1 || routes != 2 {
				t.Fatalf("catalogs=%d gateway lists=%d selections=%d routes=%d", catalogs, lists, selections, routes)
			}
			raw, err := os.ReadFile(filepath.Join(runDir, "history.json"))
			if err != nil {
				t.Fatal(err)
			}
			var state chain.State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if len(state.History) != 1 {
				t.Fatalf("unexpected history: %+v", state.History)
			}
			record := state.History[0]
			if record.Output != wantEndpoint {
				t.Fatalf("process was invoked with %q, want %q", record.Output, wantEndpoint)
			}
			if record.Model != "a/x" || record.ModelPrefix != wantPrefix {
				t.Fatalf("history recorded model=%q prefix=%q", record.Model, record.ModelPrefix)
			}
			if through && !strings.Contains(string(raw), `"model_prefix":"openrouter/"`) {
				t.Fatalf("stored history does not note the gateway prefix: %s", raw)
			}
			if strings.Contains(string(raw), "synthetic-gateway-only") {
				t.Fatal("stored history exposed the gateway credential")
			}
		})
	}
}

// Routing is an invocation as well: with a gateway configured it must reach
// the model by the same route, or the operator's routing account is unchanged.
func TestGatewayRoutesThroughTheSamePrefixAndLogsBothIDs(t *testing.T) {
	selector := gatewaySelector(t)
	var observed []string
	selector.observe = func(message string) { observed = append(observed, message) }
	chats := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x"), selectionModel("a/z")}}), nil
		case "gateway.example":
			return selectionReply(request, 200, map[string]any{"data": []any{gatewayEntry("openrouter/a/x")}}), nil
		case "selection.example":
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "a/x"}}}), nil
		case "routing.example":
			chats++
			var input struct{ Model string }
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			if input.Model != "openrouter/a/x" {
				t.Errorf("routing reached %q, not the gateway endpoint", input.Model)
			}
			return routingSelectionReply(request, chain.Assignment{Role: "review", Instruction: "Inspect the work."}), nil
		}
		return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
	})
	router := selectedChatRouter{selection: selector, chat: chain.ChatRouter{
		Service: chain.Jev{URL: "https://routing.example/chat", Model: "never-use-pinned", KeyEnv: "SELECTION_TEST_KEY"},
		Roles:   map[string]string{"review": "Independent review"},
	}}
	assignment, err := router.Next(context.Background(), chain.State{Request: "original request"})
	if err != nil || assignment.Role != "review" || chats != 1 {
		t.Fatalf("assignment=%+v err=%v chats=%d", assignment, err, chats)
	}
	if len(observed) != 1 || !strings.Contains(observed[0], "freshly selected model: a/x") || !strings.Contains(observed[0], "openrouter/a/x") {
		t.Fatalf("selected id and invocation route not both visible: %v", observed)
	}
}

// An unavailable gateway list must leave the attempt unavailable. Invoking the
// bare id would bill the account the operator is moving away from, and reusing
// an earlier list would claim an availability nobody observed.
func TestGatewayListFailureLeavesTheAttemptUnavailableWithoutBareInvocation(t *testing.T) {
	for name, reply := range map[string]func(*http.Request) (*http.Response, error){
		"unavailable": func(r *http.Request) (*http.Response, error) {
			return catalogReply(r, 503, "gateway list unavailable: synthetic-gateway-only"), nil
		},
		"timeout":    func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded },
		"unreadable": func(r *http.Request) (*http.Response, error) { return catalogReply(r, 200, `{"data":[`), nil },
		"empty":      func(r *http.Request) (*http.Response, error) { return catalogReply(r, 200, `{"data":[]}`), nil },
	} {
		t.Run(name, func(t *testing.T) {
			selector := gatewaySelector(t)
			catalogs, lists, selections := 0, 0, 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai":
					catalogs++
					return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x")}}), nil
				case "gateway.example":
					lists++
					if lists == 1 {
						return selectionReply(request, 200, map[string]any{"data": []any{gatewayEntry("openrouter/a/x")}}), nil
					}
					return reply(request)
				case "selection.example":
					selections++
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "a/x"}}}), nil
				}
				return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
			})
			role, state := chain.Role{Name: "implement"}, chain.State{Request: "original request"}
			if model, err := selector.choose(context.Background(), role, chain.Process{}, state, nil); err != nil || model != "a/x" {
				t.Fatalf("healthy attempt: model=%q err=%v", model, err)
			}
			model, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
			if err == nil || model != "" {
				t.Fatalf("failing gateway list produced %q (%v)", model, err)
			}
			if strings.Contains(err.Error(), "synthetic-gateway-only") {
				t.Fatalf("failure exposed the gateway credential: %v", err)
			}
			if catalogs != 2 || lists != 2 || selections != 1 {
				t.Fatalf("stale list or a judge asked anyway: catalogs=%d lists=%d selections=%d", catalogs, lists, selections)
			}
		})
	}
}

// Availability at the gateway changes independently of the public catalog, so
// every attempt asks again instead of trusting the previous answer.
func TestGatewayListIsFetchedForEveryAttempt(t *testing.T) {
	selector := gatewaySelector(t)
	var offered [][]string
	lists := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x"), selectionModel("a/z")}}), nil
		case "gateway.example":
			lists++
			served := "openrouter/a/x"
			if lists == 2 {
				served = "openrouter/a/z"
			}
			return selectionReply(request, 200, map[string]any{"data": []any{gatewayEntry(served)}}), nil
		case "selection.example":
			choices := offeredChoices(t, request)
			offered = append(offered, choices)
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choices[0]}}}), nil
		}
		return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
	})
	role, state := chain.Role{Name: "implement"}, chain.State{Request: "original request"}
	first, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first != "a/x" || second != "a/z" || lists != 2 {
		t.Fatalf("first=%q second=%q lists=%d", first, second, lists)
	}
	if !reflect.DeepEqual(offered, [][]string{{"a/x"}, {"a/z"}}) {
		t.Fatalf("withdrawn endpoint stayed on offer: %v", offered)
	}
}

// The chain's existing recovery handles a gateway outage: the failure reaches
// the router as an observation and the next attempt invokes through the
// gateway. No role is launched with an un-prefixed id in between.
func TestGatewayOutageReturnsToRoutingThenInvokesThroughTheGateway(t *testing.T) {
	selector := gatewaySelector(t)
	catalogs, lists, routes := 0, 0, 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			catalogs++
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x")}}), nil
		case "gateway.example":
			lists++
			if lists == 1 {
				return catalogReply(request, 503, "gateway list outage"), nil
			}
			return selectionReply(request, 200, map[string]any{"data": []any{gatewayEntry("openrouter/a/x")}}), nil
		case "selection.example":
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "a/x"}}}), nil
		case "routing.example":
			routes++
			var body struct{ State chain.State }
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			choice := "implement"
			if routes == 2 && (len(body.State.History) != 1 || !strings.Contains(body.State.History[0].Error, "gateway list outage")) {
				t.Errorf("gateway outage was hidden from recovery: %#v", body.State)
			}
			if routes == 3 {
				if len(body.State.History) != 2 || body.State.History[1].Output != "openrouter/a/x" || body.State.History[1].ModelPrefix != "openrouter/" {
					t.Errorf("recovered launch did not go through the gateway: %#v", body.State)
				}
				choice = "done"
			} else if routes > 3 {
				return nil, fmt.Errorf("unexpected additional routing")
			}
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		}
		return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
	})
	var cfg config
	cfg.Router.Mode = "jev"
	cfg.Router.Decision = chain.Jev{URL: "https://routing.example/decisions", Model: "router", KeyEnv: "SELECTION_TEST_KEY"}
	cfg.ModelSelection = &selector
	cfg.Roles = []chain.Role{{Name: "implement", Purpose: "implement", Processes: []chain.Process{{
		Name: "worker", ModelEnv: "CHOSEN_MODEL", Command: []string{"/bin/sh", "-c", `printf '%s' "$CHOSEN_MODEL"`},
	}}}}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var log bytes.Buffer
	if err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(dir, "run")}, io.Discard, &log); err != nil {
		t.Fatalf("%v\n%s", err, &log)
	}
	if catalogs != 2 || lists != 2 || routes != 3 {
		t.Fatalf("catalogs=%d gateway lists=%d routes=%d\n%s", catalogs, lists, routes, &log)
	}
}

// A half-configured gateway would quietly keep invoking the account the
// operator is moving off, so it is refused before any issue is accepted.
func TestGatewayConfigurationIsRefusedBeforeIntake(t *testing.T) {
	for name, broken := range map[string]struct {
		gateway gatewayConfig
		want    string
	}{
		"no models url": {gatewayConfig{KeyEnv: "GATEWAY_API_KEY", Prefix: "openrouter/"}, "model_selection.gateway.models_url"},
		"no key env":    {gatewayConfig{ModelsURL: "https://gateway.example.invalid/v1/models", Prefix: "openrouter/"}, "model_selection.gateway.key_env"},
		"no prefix":     {gatewayConfig{ModelsURL: "https://gateway.example.invalid/v1/models", KeyEnv: "GATEWAY_API_KEY"}, "model_selection.gateway.prefix"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := gatewayExample(t)
			cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
			gateway := broken.gateway
			cfg.ModelSelection.Gateway = &gateway
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				t.Errorf("refused configuration still reached %s", request.URL.Host)
				return nil, fmt.Errorf("unconfigured")
			})
			dir := t.TempDir()
			configPath := filepath.Join(dir, "operator.json")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "must-not-be-created")
			var log bytes.Buffer
			// Bound the run: an accepted configuration would poll for intake
			// until the whole suite times out, which reports nothing useful.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = run(ctx, []string{"--config", configPath, "--watch", "--run-dir", root}, io.Discard, &log)
			if err == nil || !strings.Contains(err.Error(), broken.want) {
				t.Fatalf("half-configured gateway was accepted: %v", err)
			}
			if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), ". ") {
				t.Fatalf("refusal is not one sentence: %q", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("refused configuration created the queue")
			}
		})
	}
}

// The alternative selector chooses from the same gateway-served set: an
// unavailable primary selector must not widen what this account can invoke.
func TestGatewayNarrowsTheAlternativeSelectorToo(t *testing.T) {
	selector := gatewaySelector(t)
	selector.Fallback = &chain.Jev{URL: "https://chat-selection.example/chat", Model: "configured-alternative", KeyEnv: "SELECTION_TEST_KEY"}
	lists, primary, alternative := 0, 0, 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x"), selectionModel("a/z")}}), nil
		case "gateway.example":
			lists++
			return selectionReply(request, 200, map[string]any{"data": []any{gatewayEntry("openrouter/a/z")}}), nil
		case "selection.example":
			primary++
			return catalogReply(request, 503, "primary selector unavailable"), nil
		case "chat-selection.example":
			alternative++
			var input struct {
				Tools []struct {
					Function struct {
						Parameters struct {
							Properties map[string]struct{ Enum []string }
						}
					}
				}
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			if got := input.Tools[0].Function.Parameters.Properties["role"].Enum; !reflect.DeepEqual(got, []string{"a/z"}) {
				t.Errorf("alternative selector was offered %v, not only what the gateway serves", got)
			}
			return selectionReply(request, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"tool_calls": []any{map[string]any{"function": map[string]string{"name": "handoff", "arguments": `{"role":"a/z"}`}}},
			}}}}), nil
		}
		return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
	})
	model, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
	if err != nil || model != "a/z" || lists != 2 || primary != 1 || alternative != 1 {
		t.Fatalf("model=%q err=%v gateway lists=%d primary=%d alternative=%d", model, err, lists, primary, alternative)
	}
}

// model_selection.fixed names one model for every launch. It is an experiment
// switch for comparing a single strong model against per-launch selection.
func TestFixedModelSkipsEveryLookupAndInvokesThroughTheGateway(t *testing.T) {
	for _, through := range []bool{false, true} {
		t.Run(fmt.Sprintf("gateway=%t", through), func(t *testing.T) {
			selector := gatewaySelector(t)
			selector.Fixed = "publisher/one-strong-model"
			if !through {
				selector.Gateway = nil
			}
			routes := 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai", "gateway.example":
					t.Errorf("a named model still fetched a list from %s", request.URL.Host)
					return nil, fmt.Errorf("unexpected list request")
				case "selection.example":
					t.Error("a named model still asked the selector to choose")
					return nil, fmt.Errorf("unexpected selection request")
				case "routing.example":
					routes++
					choice := "review"
					if routes == 2 {
						choice = "done"
					} else if routes > 2 {
						return nil, fmt.Errorf("unexpected additional routing")
					}
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
				}
				return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
			})
			var cfg config
			cfg.Router.Mode = "jev"
			cfg.Router.Decision = chain.Jev{URL: "https://routing.example/decisions", Model: "router", KeyEnv: "SELECTION_TEST_KEY"}
			cfg.ModelSelection = &selector
			// Two peers in one group: a named model cannot be excluded, so both
			// receive it. The operator is choosing that, not losing separation.
			worker := chain.Process{ModelEnv: "CHOSEN_MODEL", Command: []string{"/bin/sh", "-c", `printf '%s' "$CHOSEN_MODEL"`}}
			first, second := worker, worker
			first.Name, second.Name = "reviewer-a", "reviewer-b"
			cfg.Roles = []chain.Role{{Name: "review", Purpose: "independent review", Processes: []chain.Process{first, second}}}
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var log bytes.Buffer
			runDir := filepath.Join(dir, "run")
			if err := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", runDir}, io.Discard, &log); err != nil {
				t.Fatalf("%v\n%s", err, &log)
			}
			wantEndpoint, wantPrefix := "publisher/one-strong-model", ""
			if through {
				wantEndpoint, wantPrefix = "openrouter/publisher/one-strong-model", "openrouter/"
			}
			raw, err := os.ReadFile(filepath.Join(runDir, "history.json"))
			if err != nil {
				t.Fatal(err)
			}
			var state chain.State
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			if len(state.History) != 2 {
				t.Fatalf("both peers did not run: %+v", state.History)
			}
			for _, record := range state.History {
				if record.Output != wantEndpoint {
					t.Fatalf("%s was invoked with %q, want %q", record.Speaker, record.Output, wantEndpoint)
				}
				if record.Model != "publisher/one-strong-model" || record.ModelPrefix != wantPrefix {
					t.Fatalf("history recorded model=%q prefix=%q", record.Model, record.ModelPrefix)
				}
			}
		})
	}
}

// A named model wins over configured publishers, and whitespace alone is not
// a name: it must leave per-launch selection exactly as it was.
func TestFixedModelOverridesPublishersAndBlankReadsAsAbsent(t *testing.T) {
	for name, fixed := range map[string]string{"named": "publisher/one-strong-model", "blank": "   "} {
		t.Run(name, func(t *testing.T) {
			selector := gatewaySelector(t)
			selector.Gateway, selector.Fixed = nil, fixed
			selector.Authors = []string{"a"}
			catalogs, selections := 0, 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				switch request.URL.Host {
				case "openrouter.ai":
					catalogs++
					return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("a/x")}}), nil
				case "selection.example":
					selections++
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "a/x"}}}), nil
				}
				return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
			})
			model, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
			wantModel, wantCalls := "publisher/one-strong-model", 0
			if name == "blank" {
				wantModel, wantCalls = "a/x", 1
			}
			if err != nil || model != wantModel || catalogs != wantCalls || selections != wantCalls {
				t.Fatalf("model=%q err=%v catalogs=%d selections=%d", model, err, catalogs, selections)
			}
		})
	}
}

// A name that is not a full endpoint id would only fail at launch, so it is
// refused with the rest of the configuration.
func TestFixedModelWithoutAPublisherIsRefusedBeforeIntake(t *testing.T) {
	cfg := gatewayExample(t)
	cfg.Intake.ProjectID, cfg.Intake.CreatedSince = 17, "2026-01-02T00:00:00Z"
	cfg.ModelSelection.Fixed = "one-strong-model"
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		t.Errorf("refused configuration still reached %s", request.URL.Host)
		return nil, fmt.Errorf("unconfigured")
	})
	dir := t.TempDir()
	configPath := filepath.Join(dir, "operator.json")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "must-not-be-created")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var log bytes.Buffer
	err = run(ctx, []string{"--config", configPath, "--watch", "--run-dir", root}, io.Discard, &log)
	if err == nil || !strings.Contains(err.Error(), "model_selection.fixed") {
		t.Fatalf("an endpoint id without a publisher was accepted: %v", err)
	}
	if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), ". ") {
		t.Fatalf("refusal is not one sentence: %q", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("refused configuration created the queue")
	}
}
