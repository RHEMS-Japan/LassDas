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

func selectionModel(id string) map[string]any {
	return map[string]any{"id": id, "name": id, "canonical_slug": id + "-provider-version", "created": 1234,
		"pricing":              map[string]string{"prompt": "0.000001", "completion": "0.000002"},
		"supported_parameters": []string{"tools"},
		"architecture":         map[string]any{"output_modalities": []string{"text"}},
		"description":          "current provider description"}
}

func selectionReply(request *http.Request, code int, value any) *http.Response {
	body, _ := json.Marshal(value)
	return catalogReply(request, code, string(body))
}

func testSelector(t *testing.T) selectionConfig {
	t.Helper()
	t.Setenv("SELECTION_TEST_KEY", "synthetic-selection-only")
	return selectionConfig{Judge: chain.Jev{URL: "https://selection.example/decisions", Model: "decision-model", KeyEnv: "SELECTION_TEST_KEY"}, Authors: []string{"maker-one", "maker-two"}}
}

func TestSelectionFetchesPerProcessAndUsesCurrentIndependentPublishers(t *testing.T) {
	selector := testSelector(t)
	catalogCalls, decisions := 0, 0
	original := "Implement the original request, 日本語; do not reinterpret it."
	state := chain.State{Request: original, History: []chain.Result{{Role: "implement", Model: "maker-one/earlier", Error: "model service returned HTTP 503", Output: "full working prose stays in the role prompt"}}}
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			catalogCalls++
			if request.Header.Get("Authorization") != "" || request.Header.Get("Cache-Control") != "no-cache" {
				t.Error("catalog was not refreshed without a credential")
			}
			plain := selectionModel("maker-one/no-tools")
			delete(plain, "supported_parameters")
			return selectionReply(request, 200, map[string]any{"data": []any{
				selectionModel(fmt.Sprintf("maker-one/current-%d", catalogCalls)),
				selectionModel(fmt.Sprintf("maker-two/current-%d", catalogCalls)),
				selectionModel("outside/current"), plain,
			}}), nil
		case "selection.example":
			decisions++
			if request.Header.Get("Authorization") != "Bearer synthetic-selection-only" {
				t.Error("selector did not use its own named credential")
			}
			var input struct {
				State     chain.State
				Questions map[string]struct {
					Instructions string
					Criteria     map[string]string
				}
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				return nil, err
			}
			if input.State.Request != original || len(input.State.History) != 1 || input.State.History[0].Error != state.History[0].Error || input.State.History[0].Model != "maker-one/earlier" || input.State.History[0].Output != "" {
				t.Errorf("selection lost the request/runtime failure or copied unrelated prose: %#v", input.State)
			}
			question := input.Questions["next"]
			if !strings.Contains(question.Instructions, "independent review") || !strings.Contains(question.Instructions, "Catalog observation:") {
				t.Error("selection responsibility or observation time missing")
			}
			choice := fmt.Sprintf("maker-one/current-%d", catalogCalls)
			if decisions == 2 {
				choice = "maker-two/current-2"
			}
			want := map[string]bool{choice: true}
			if decisions != 2 {
				want[fmt.Sprintf("maker-two/current-%d", catalogCalls)] = true
			}
			got := map[string]bool{}
			for id, metadata := range question.Criteria {
				got[id] = true
				if !strings.Contains(metadata, "0.000002") || !strings.Contains(metadata, "current provider description") || !strings.Contains(metadata, "1970-01-01T00:20:34Z") || !strings.Contains(metadata, "-provider-version") {
					t.Error("current selection metadata missing")
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("eligible endpoints=%v want=%v", got, want)
			}
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
		default:
			return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
		}
	})
	role := chain.Role{Name: "review", Purpose: "independent review"}
	first, err := selector.choose(context.Background(), role, chain.Process{Name: "a"}, state, nil)
	if err != nil || first != "maker-one/current-1" {
		t.Fatalf("first=%q error=%v", first, err)
	}
	second, err := selector.choose(context.Background(), role, chain.Process{Name: "b"}, state, []string{first})
	if err != nil || second != "maker-two/current-2" {
		t.Fatalf("second=%q error=%v", second, err)
	}
	third, err := selector.choose(context.Background(), role, chain.Process{Name: "a"}, state, nil)
	if err != nil || third != "maker-one/current-3" || catalogCalls != 3 || decisions != 3 {
		t.Fatalf("next launch reused a selection: %q %v catalogs=%d decisions=%d", third, err, catalogCalls, decisions)
	}
}

func TestSelectionFailureNeverUsesPinnedOrEarlierModel(t *testing.T) {
	for _, failure := range []string{"catalog", "selection", "unknown-choice", "no-independent-model"} {
		t.Run(failure, func(t *testing.T) {
			selector := testSelector(t)
			calls := 0
			useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Host == "openrouter.ai" {
					calls++
					if calls == 2 && failure == "catalog" {
						return catalogReply(request, 503, "current catalog unavailable"), nil
					}
					return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("maker-one/current")}}), nil
				}
				if calls == 2 && failure == "selection" {
					return catalogReply(request, 429, "retry later"), nil
				}
				choice := "maker-one/current"
				if calls == 2 && failure == "unknown-choice" {
					choice = "maker-one/retired"
				}
				return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
			})
			role := chain.Role{Name: "implement"}
			if _, err := selector.choose(context.Background(), role, chain.Process{}, chain.State{Request: "original"}, nil); err != nil {
				t.Fatal(err)
			}
			var selected []string
			if failure == "no-independent-model" {
				selected = []string{"maker-one/any-version"}
			}
			model, err := selector.choose(context.Background(), role, chain.Process{}, chain.State{Request: "original"}, selected)
			if err == nil || model != "" || calls != 2 {
				t.Fatalf("failure substituted model %q, error=%v calls=%d", model, err, calls)
			}
		})
	}
}

func TestExecutableSelectionOutageReturnsToRoutingThenLaunchesFreshModel(t *testing.T) {
	selector := testSelector(t)
	dir := t.TempDir()
	catalogs, routes, selections := 0, 0, 0
	useCatalogTransport(t, func(request *http.Request) (*http.Response, error) {
		switch request.URL.Host {
		case "openrouter.ai":
			catalogs++
			if catalogs == 1 {
				return catalogReply(request, 503, "fresh-list outage"), nil
			}
			return selectionReply(request, 200, map[string]any{"data": []any{selectionModel("maker-one/now")}}), nil
		case "selection.example":
			selections++
			return selectionReply(request, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": "maker-one/now"}}}), nil
		case "routing.example":
			routes++
			var body struct{ State chain.State }
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body.State.Request != "original request" {
				t.Error("original request changed")
			}
			choice := "implement"
			if routes == 2 && (len(body.State.History) != 1 || !strings.Contains(body.State.History[0].Error, "fresh-list outage")) {
				t.Errorf("selection outage was hidden: %#v", body.State)
			}
			if routes == 3 {
				if len(body.State.History) != 2 || body.State.History[1].Model != "maker-one/now" || body.State.History[1].Output != "maker-one/now\nordinary prose without a schema\n" {
					t.Errorf("current model did not reach the real process: %#v", body.State)
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
	cfg.Roles = []chain.Role{{Name: "implement", Purpose: "implement", Processes: []chain.Process{{Name: "worker", ModelEnv: "CHOSEN_MODEL", Env: map[string]string{"CHOSEN_MODEL": "stale/pinned"}, Command: []string{"/bin/sh", "-c", `test -z "$SELECTION_TEST_KEY" || exit 9; printf '%s\n' "$CHOSEN_MODEL" 'ordinary prose without a schema'`}}}}}
	data, _ := json.Marshal(cfg)
	configPath, requestPath := filepath.Join(dir, "operator.json"), filepath.Join(dir, "request.txt")
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
	if catalogs != 2 || routes != 3 || selections != 1 {
		t.Fatalf("catalogs=%d routes=%d selections=%d", catalogs, routes, selections)
	}
}
