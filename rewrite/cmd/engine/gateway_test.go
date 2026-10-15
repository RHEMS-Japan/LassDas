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
	"regexp"
	"slices"
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
			if model, _, err := selector.choose(context.Background(), role, chain.Process{}, state, nil); err != nil || model != "a/x" {
				t.Fatalf("healthy attempt: model=%q err=%v", model, err)
			}
			model, _, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
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
	first, _, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := selector.choose(context.Background(), role, chain.Process{}, state, nil)
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
	model, _, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
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
			model, _, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
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

// gatewayHosts answers the public catalog with catalog, in that order, the
// gateway list with served, and the judge with the first of prefer it was
// offered. Every offer is kept, sorted, in the order the judge was asked.
func gatewayHosts(t *testing.T, catalog []string, served []any, prefer []string, offers *[][]string) roundTripFunc {
	t.Helper()
	return func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			var data []any
			for _, id := range catalog {
				data = append(data, selectionModel(id))
			}
			return selectionReply(request, 200, map[string]any{"data": data}), nil
		case "gateway.example":
			return selectionReply(request, 200, map[string]any{"data": served}), nil
		case "selection.example":
			offered := offeredChoices(t, request)
			*offers = append(*offers, offered)
			for _, id := range prefer {
				if slices.Contains(offered, id) {
					return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": id}}}), nil
				}
			}
			return catalogReply(request, 503, "none of the preferred models was offered"), nil
		}
		return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
	}
}

// listedEntries is a gateway list of ids, each with the owner the gateway
// names as gatewayEntry gives it.
func listedEntries(ids ...string) []any {
	var entries []any
	for _, id := range ids {
		entries = append(entries, gatewayEntry(id))
	}
	return entries
}

// nameOnlySelector reaches the catalog's models through a gateway that lists
// them under the prefix p/ without the publisher: "p/glm-5.3" for the
// catalog's "z-ai/glm-5.3".
func nameOnlySelector(t *testing.T) selectionConfig {
	t.Helper()
	selector := gatewaySelector(t)
	selector.Authors = []string{"deepseek", "minimax", "z-ai"}
	selector.Gateway.Prefix = "p/"
	return selector
}

// runRoleOnce runs one request that the router sends to role once and then
// finishes, and returns its saved history, as read and as stored.
func runRoleOnce(t *testing.T, selector selectionConfig, role chain.Role, hosts roundTripFunc) (chain.State, string, error) {
	t.Helper()
	routes := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "routing.example" {
			return hosts(request)
		}
		routes++
		choice := role.Name
		if routes > 1 {
			choice = "done"
		}
		return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
	})
	var cfg config
	cfg.Router.Mode = "jev"
	cfg.Router.Decision = chain.Jev{URL: "https://routing.example/decisions", Model: "router", KeyEnv: "SELECTION_TEST_KEY"}
	cfg.ModelSelection = &selector
	cfg.Roles = []chain.Role{role}
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
	runErr := run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", runDir}, io.Discard, &log)
	if runErr != nil {
		runErr = fmt.Errorf("%w\n%s", runErr, &log)
	}
	raw, err := os.ReadFile(filepath.Join(runDir, "history.json"))
	if err != nil {
		t.Fatalf("no history (%v): %v", runErr, err)
	}
	var state chain.State
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	return state, string(raw), runErr
}

// A gateway that lists its models without the publisher, as "p/glm-5.3" for
// the catalog's "z-ai/glm-5.3", served nothing before: every candidate was
// looked for as "p/z-ai/glm-5.3", none was found, and no role could start.
// The catalog still decides what is eligible and is what the judge reads and
// the history records; the role is handed the id the gateway lists, and the
// record keeps that id beside the catalog id and the prefix.
func TestAGatewayThatListsModelsWithoutThePublisherServesThem(t *testing.T) {
	var offers [][]string
	worker := chain.Process{Name: "worker", ModelEnv: "CHOSEN_MODEL", Command: []string{"/bin/sh", "-c", `test -z "$GATEWAY_TEST_KEY" || exit 9; printf '%s' "$CHOSEN_MODEL"`}}
	state, raw, err := runRoleOnce(t, nameOnlySelector(t), chain.Role{Name: "implement", Purpose: "implement", Processes: []chain.Process{worker}},
		gatewayHosts(t, []string{"deepseek/deepseek-v4-pro", "z-ai/glm-5.3", "z-ai/glm-5.3-flash"},
			listedEntries("p/deepseek-v4-pro", "p/glm-5.3", "p/kling-video-v3"), []string{"z-ai/glm-5.3"}, &offers))
	if want := [][]string{{"deepseek/deepseek-v4-pro", "z-ai/glm-5.3"}}; !reflect.DeepEqual(offers, want) {
		t.Fatalf("the judge was offered %v, want %v; history %+v", offers, want, state.History)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(state.History) != 1 {
		t.Fatalf("unexpected history: %+v", state.History)
	}
	if record := state.History[0]; record.Output != "p/glm-5.3" || record.Model != "z-ai/glm-5.3" || record.ModelPrefix != "p/" || record.Invoked != "p/glm-5.3" {
		t.Fatalf("invoked with %q, recorded model=%q prefix=%q invoked=%q", record.Output, record.Model, record.ModelPrefix, record.Invoked)
	}
	if strings.Contains(raw, "synthetic-gateway-only") {
		t.Fatal("stored history exposed the gateway credential")
	}
}

// Routing is invoked the same way, by the id the gateway lists, and the log
// names both the catalog id and that id.
func TestRoutingReachesAModelByTheIDAGatewayListsWithoutThePublisher(t *testing.T) {
	selector := nameOnlySelector(t)
	var observed []string
	selector.observe = func(message string) { observed = append(observed, message) }
	var offers [][]string
	hosts := gatewayHosts(t, []string{"z-ai/glm-5.3"}, listedEntries("p/glm-5.3"), []string{"z-ai/glm-5.3"}, &offers)
	chats := 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "routing.example" {
			return hosts(request)
		}
		chats++
		var input struct{ Model string }
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			return nil, err
		}
		if input.Model != "p/glm-5.3" {
			t.Errorf("routing reached %q, not the id the gateway lists", input.Model)
		}
		return routingSelectionReply(request, chain.Assignment{Role: "review", Instruction: "Inspect the work."}), nil
	})
	router := selectedChatRouter{selection: selector, chat: chain.ChatRouter{
		Service: chain.Jev{URL: "https://routing.example/chat", Model: "never-use-pinned", KeyEnv: "SELECTION_TEST_KEY"},
		Roles:   map[string]string{"review": "Independent review"},
	}}
	assignment, err := router.Next(context.Background(), chain.State{Request: "original request"})
	if err != nil || assignment.Role != "review" || chats != 1 {
		t.Fatalf("assignment=%+v err=%v chats=%d offers=%v", assignment, err, chats, offers)
	}
	if want := []string{"routing with freshly selected model: z-ai/glm-5.3; invoked through the configured gateway as p/glm-5.3"}; !reflect.DeepEqual(observed, want) {
		t.Fatalf("routing log: %q", observed)
	}
}

// A gateway that lists a model both ways is reached the way the catalog names
// it, as it always was, and the record holds nothing new.
func TestAGatewayListingBothShapesKeepsThePublisherShape(t *testing.T) {
	var offers [][]string
	worker := chain.Process{Name: "worker", ModelEnv: "CHOSEN_MODEL", Command: []string{"/bin/sh", "-c", `printf '%s' "$CHOSEN_MODEL"`}}
	state, raw, err := runRoleOnce(t, nameOnlySelector(t), chain.Role{Name: "implement", Purpose: "implement", Processes: []chain.Process{worker}},
		gatewayHosts(t, []string{"z-ai/glm-5.3"}, listedEntries("p/glm-5.3", "p/z-ai/glm-5.3"), []string{"z-ai/glm-5.3"}, &offers))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(offers, [][]string{{"z-ai/glm-5.3"}}) || len(state.History) != 1 {
		t.Fatalf("offers=%v history=%+v", offers, state.History)
	}
	if record := state.History[0]; record.Output != "p/z-ai/glm-5.3" || record.Model != "z-ai/glm-5.3" || record.ModelPrefix != "p/" || record.Invoked != "" {
		t.Fatalf("invoked with %q, recorded model=%q prefix=%q invoked=%q", record.Output, record.Model, record.ModelPrefix, record.Invoked)
	}
	if strings.Contains(raw, `"invoked"`) {
		t.Fatalf("the record gained a field: %s", raw)
	}
}

// Peers in a review group run on different publishers, behind ids without
// the publisher too: the second reviewer is not given another z-ai model.
func TestPeersReachedByIDsWithoutThePublisherStillDifferInPublisher(t *testing.T) {
	var offers [][]string
	worker := chain.Process{ModelEnv: "CHOSEN_MODEL", Command: []string{"/bin/sh", "-c", `printf '%s' "$CHOSEN_MODEL"`}}
	first, second := worker, worker
	first.Name, second.Name = "reviewer-a", "reviewer-b"
	catalog := []string{"deepseek/deepseek-v4-pro", "z-ai/glm-5.3", "z-ai/glm-5.3-flash"}
	state, _, err := runRoleOnce(t, nameOnlySelector(t), chain.Role{Name: "review", Purpose: "independent review", Processes: []chain.Process{first, second}},
		gatewayHosts(t, catalog, listedEntries("p/deepseek-v4-pro", "p/glm-5.3", "p/glm-5.3-flash"),
			[]string{"z-ai/glm-5.3", "z-ai/glm-5.3-flash", "deepseek/deepseek-v4-pro"}, &offers))
	if want := [][]string{catalog, {"deepseek/deepseek-v4-pro"}}; !reflect.DeepEqual(offers, want) {
		t.Fatalf("the judge was offered %v, want %v; history %+v", offers, want, state.History)
	}
	if err != nil {
		t.Fatal(err)
	}
	var invoked []string
	for _, record := range state.History {
		invoked = append(invoked, record.Speaker+"="+record.Model+"@"+record.Output)
	}
	if want := []string{"reviewer-a=z-ai/glm-5.3@p/glm-5.3", "reviewer-b=deepseek/deepseek-v4-pro@p/deepseek-v4-pro"}; !reflect.DeepEqual(invoked, want) {
		t.Fatalf("peers ran %v, want %v", invoked, want)
	}
}

// A model name alone can belong to more than one publisher. The gateway's
// own word on the owner settles it only when it names a configured
// publisher; any other owner, or none, settles nothing, and then a name the
// catalog gives more than one id is not taken as any of them.
func TestAGatewayIDWithoutThePublisherIsTakenOnlyWhenItsPublisherIsKnown(t *testing.T) {
	entry := func(id string, owner any) map[string]any {
		listed := map[string]any{"id": id, "object": "model"}
		if owner != nil {
			listed["owned_by"] = owner
		}
		return listed
	}
	for _, shape := range []struct {
		name     string
		catalog  []string
		owner    any
		offered  []string
		endpoint string
	}{
		{"two publishers, no owner", []string{"maker-one/shared", "maker-two/shared", "maker-one/other"}, nil, []string{"maker-one/other"}, "p/other"},
		{"two publishers, owner names the first", []string{"maker-one/shared", "maker-two/shared", "maker-one/other"}, "maker-one", []string{"maker-one/other", "maker-one/shared"}, "p/shared"},
		{"two publishers, owner names the second in capitals", []string{"maker-one/shared", "maker-two/shared", "maker-one/other"}, "MAKER-TWO", []string{"maker-one/other", "maker-two/shared"}, "p/shared"},
		{"a publisher outside the configured ones shares the name", []string{"maker-one/shared", "outsider/shared", "maker-one/other"}, nil, []string{"maker-one/other"}, "p/other"},
		{"one publisher, owner is the provider", []string{"maker-one/shared", "maker-one/other"}, "p", []string{"maker-one/other", "maker-one/shared"}, "p/shared"},
		{"one publisher, owner is a company nobody configured", []string{"maker-one/shared", "maker-one/other"}, "somecorp", []string{"maker-one/other", "maker-one/shared"}, "p/shared"},
		{"one publisher, owner publishes in the catalog but is not configured", []string{"maker-one/shared", "outsider/elsewhere", "maker-one/other"}, "outsider", []string{"maker-one/other", "maker-one/shared"}, "p/shared"},
		{"one publisher, owner is not a string", []string{"maker-one/shared", "maker-one/other"}, 7, []string{"maker-one/other", "maker-one/shared"}, "p/shared"},
		{"one publisher, owner names another configured publisher", []string{"maker-one/shared", "maker-one/other"}, "maker-two", []string{"maker-one/other"}, "p/other"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			selector := nameOnlySelector(t)
			selector.Authors = []string{"maker-one", "maker-two"}
			var offers [][]string
			useCatalogTransport(t, gatewayHosts(t, shape.catalog, []any{entry("p/shared", shape.owner), entry("p/other", nil)},
				[]string{"maker-one/shared", "maker-two/shared", "maker-one/other"}, &offers))
			_, endpoint, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
			if err != nil || endpoint != shape.endpoint || !reflect.DeepEqual(offers, [][]string{shape.offered}) {
				t.Fatalf("endpoint=%q err=%v offered %v, want %q offered %v", endpoint, err, offers, shape.endpoint, shape.offered)
			}
		})
	}
}

// The gateway's word on the owner, when it names a configured publisher, is
// taken over the name: "p/glm-5.3" owned by minimax is not z-ai's glm-5.3.
func TestAGatewayIDOwnedByAnotherConfiguredPublisherIsNotTaken(t *testing.T) {
	selector := nameOnlySelector(t)
	var offers [][]string
	useCatalogTransport(t, gatewayHosts(t, []string{"z-ai/glm-5.3"},
		[]any{map[string]any{"id": "p/glm-5.3", "owned_by": "minimax"}}, []string{"z-ai/glm-5.3"}, &offers))
	model, endpoint, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
	if err == nil || model != "" || endpoint != "" || len(offers) != 0 {
		t.Fatalf("model=%q endpoint=%q err=%v offers=%v", model, endpoint, err, offers)
	}
}

// Ids are compared without regard to case, and the role is handed the id as
// the gateway lists it, in either shape.
func TestGatewayIDsAreMatchedWithoutRegardToCaseAndInvokedAsListed(t *testing.T) {
	for listed, want := range map[string]string{"p/MiniMax-M2.5": "p/MiniMax-M2.5", "P/MiniMax/MiniMax-M2.5": "P/MiniMax/MiniMax-M2.5"} {
		t.Run(listed, func(t *testing.T) {
			var offers [][]string
			useCatalogTransport(t, gatewayHosts(t, []string{"minimax/minimax-m2.5"}, listedEntries(listed), []string{"minimax/minimax-m2.5"}, &offers))
			selector := nameOnlySelector(t)
			model, endpoint, err := selector.choose(context.Background(), chain.Role{Name: "implement"}, chain.Process{}, chain.State{Request: "original request"}, nil)
			if err != nil || model != "minimax/minimax-m2.5" || endpoint != want {
				t.Fatalf("model=%q endpoint=%q err=%v offers=%v", model, endpoint, err, offers)
			}
		})
	}
}

// observedOutputs runs one request through the queue under a named shape of
// configuration and returns what a person reads of it: the request's
// history.json with its times blanked, and every comment the tracker got.
// The worker prints the id it was handed. It uses only helpers main's tests
// have, so main's engine can write the values these are compared with.
func observedOutputs(t *testing.T, shape string) (string, []string) {
	t.Helper()
	cfg := declaringRun(t, `printf '%s' "$MODEL"`)
	t.Setenv("SELECTION_TEST_KEY", "synthetic-selection-only")
	t.Setenv("GATEWAY_TEST_KEY", "synthetic-gateway-only")
	selector := selectionConfig{Judge: chain.Jev{URL: "https://selection.example/decisions", Model: "decision-model", KeyEnv: "SELECTION_TEST_KEY"},
		Authors: []string{"z-ai"}}
	gateway := &gatewayConfig{ModelsURL: "https://gateway.example/v1/models", KeyEnv: "GATEWAY_TEST_KEY", Prefix: "p/"}
	var listed []string
	switch shape {
	case "no gateway":
	case "publisher listed":
		selector.Gateway, listed = gateway, []string{"p/z-ai/glm-5.3", "p/model-of-its-own"}
	case "fixed through a gateway":
		selector.Gateway, selector.Fixed = gateway, "z-ai/glm-5.3"
	case "name only listed":
		selector.Gateway, listed = gateway, []string{"p/glm-5.3"}
	default:
		t.Fatalf("unknown shape %q", shape)
	}
	cfg.ModelSelection = &selector
	lookEvery(t, 20*time.Millisecond)
	fixture := &noticeTracker{}
	fixture.install(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("z-ai/glm-5.3")}}), nil
		case "gateway.example":
			var data []any
			for _, id := range listed {
				data = append(data, gatewayEntry(id))
			}
			return selectionReply(request, 200, map[string]any{"data": data}), nil
		case "selection.example":
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "z-ai/glm-5.3"}}}), nil
		}
		return alwaysChoose("done")(request)
	})
	root, directory := noticeJob(t, chain.State{})
	startStopQueue(t, cfg, root, time.Minute, io.Discard)
	waitFor(t, func() bool { return loadJobState(t, directory).Done && len(fixture.all()) > 0 })
	time.Sleep(50 * time.Millisecond)
	raw, err := os.ReadFile(filepath.Join(directory, "run", "history.json"))
	if err != nil {
		t.Fatal(err)
	}
	times := regexp.MustCompile(`"(started_at|finished_at)":"[^"]*"`)
	return times.ReplaceAllString(string(raw), `"$1":"-"`), fixture.all()
}

// mainObservedOutputs is what main's engine (351ee9c) wrote through
// observedOutputs, run twice with the same result: the history.json with
// its times blanked and the comments the tracker got.
const mainObservedOutputs = `{
  "fixed through a gateway": {
    "comments": [
      "作業を始めます。（モデル: z-ai/glm-5.3）"
    ],
    "history": "{\"request\":\"Original issue: EXAMPLE-51\\nTitle: Original title\\n\\nOriginal conditions\",\"history\":[{\"role\":\"work\",\"speaker\":\"worker\",\"model\":\"z-ai/glm-5.3\",\"model_prefix\":\"p/\",\"output\":\"p/z-ai/glm-5.3\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"work\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage work, written by the engine from what it observed.\\nProcess worker exited 0.\\n\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"build\",\"output\":\"\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage verify, written by the engine from what it observed.\\nProcess build exited 0.\\n\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"}],\"done\":true,\"workflow\":{\"start\":null,\"after\":{},\"recover\":{},\"stages\":[{\"name\":\"work\",\"kind\":\"model\"},{\"name\":\"verify\",\"kind\":\"command\",\"on_failure\":\"work\"}]},\"step\":\"verify\"}"
  },
  "no gateway": {
    "comments": [
      "作業を始めます。（モデル: z-ai/glm-5.3）"
    ],
    "history": "{\"request\":\"Original issue: EXAMPLE-51\\nTitle: Original title\\n\\nOriginal conditions\",\"history\":[{\"role\":\"work\",\"speaker\":\"worker\",\"model\":\"z-ai/glm-5.3\",\"output\":\"z-ai/glm-5.3\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"work\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage work, written by the engine from what it observed.\\nProcess worker exited 0.\\n\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"build\",\"output\":\"\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage verify, written by the engine from what it observed.\\nProcess build exited 0.\\n\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"}],\"done\":true,\"workflow\":{\"start\":null,\"after\":{},\"recover\":{},\"stages\":[{\"name\":\"work\",\"kind\":\"model\"},{\"name\":\"verify\",\"kind\":\"command\",\"on_failure\":\"work\"}]},\"step\":\"verify\"}"
  },
  "publisher listed": {
    "comments": [
      "作業を始めます。（モデル: z-ai/glm-5.3）"
    ],
    "history": "{\"request\":\"Original issue: EXAMPLE-51\\nTitle: Original title\\n\\nOriginal conditions\",\"history\":[{\"role\":\"work\",\"speaker\":\"worker\",\"model\":\"z-ai/glm-5.3\",\"model_prefix\":\"p/\",\"output\":\"p/z-ai/glm-5.3\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"work\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage work, written by the engine from what it observed.\\nProcess worker exited 0.\\n\",\"instruction\":\"Stage 1 of 2 in the configured run: work.\\nNothing you write is read, decoded or graded, and saying the work is done advances nothing. The verify stage runs afterwards, and what its commands return is what carries this work on.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"build\",\"output\":\"\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"},{\"role\":\"verify\",\"speaker\":\"runtime\",\"output\":\"Runtime record for stage verify, written by the engine from what it observed.\\nProcess build exited 0.\\n\",\"instruction\":\"Stage 2 of 2 in the configured run: verify.\\nThe runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\\n\",\"started_at\":\"-\",\"finished_at\":\"-\"}],\"done\":true,\"workflow\":{\"start\":null,\"after\":{},\"recover\":{},\"stages\":[{\"name\":\"work\",\"kind\":\"model\"},{\"name\":\"verify\",\"kind\":\"command\",\"on_failure\":\"work\"}]},\"step\":\"verify\"}"
  }
}`

// Where the gateway lists the publisher, where there is no gateway, and where
// a fixed model goes through a gateway, nothing a person reads changes: the
// history.json and the comment declaring the model are main's, byte for
// byte. A gateway that lists the model by its name alone is invoked by
// another id, which the record shows, while the requester is still told the
// catalog's model.
func TestNothingAPersonReadsChangesWhereTheGatewayListsThePublisher(t *testing.T) {
	var want map[string]struct {
		History  string
		Comments []string
	}
	if err := json.Unmarshal([]byte(mainObservedOutputs), &want); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{"no gateway", "publisher listed", "fixed through a gateway"} {
		t.Run(shape, func(t *testing.T) {
			history, comments := observedOutputs(t, shape)
			if history != want[shape].History {
				t.Errorf("history.json differs from main's\n got: %s\nwant: %s", history, want[shape].History)
			}
			if !reflect.DeepEqual(comments, want[shape].Comments) {
				t.Errorf("the requester was told %q, main told %q", comments, want[shape].Comments)
			}
		})
	}
	t.Run("a gateway that lists the name alone", func(t *testing.T) {
		history, comments := observedOutputs(t, "name only listed")
		if history == want["publisher listed"].History || !strings.Contains(history, `"output":"p/glm-5.3"`) || !strings.Contains(history, `"invoked":"p/glm-5.3"`) {
			t.Fatalf("the comparison did not see the other id: %s", history)
		}
		if !reflect.DeepEqual(comments, want["publisher listed"].Comments) {
			t.Fatalf("the requester was told %q, not the catalog's model", comments)
		}
	})
}
