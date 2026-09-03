package attendant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

func TestRoleProbesCollapseDuplicatesAndReadTheEnvironment(t *testing.T) {
	models := worker.ModelConfig{
		Implementer: worker.ModelEndpoint{Model: "gemini", BaseURL: "https://gw/api/v1", APIKeyEnv: "K_RECEPTION"},
		Readiness: worker.ReadinessModels{
			Assessor: worker.ModelEndpoint{Model: "gemini", BaseURL: "https://gw/api/v1", APIKeyEnv: "K_RECEPTION"},
			Checker:  worker.ModelEndpoint{Model: "deepseek", BaseURL: "https://gw/api/v1", APIKeyEnv: "K_REVIEW"},
		},
		Reviewers: []worker.ModelEndpoint{{ID: "review-a", Model: "deepseek", BaseURL: "https://gw/api/v1", APIKeyEnv: "K_REVIEW"}},
	}
	env := map[string]string{
		"LASSDAS_GATEWAY_BASE_URL": "https://gw/api/v1", "LASSDAS_IMPLEMENTER_MODEL": "opus", "LASSDAS_REVIEW_A_MODEL": "deepseek",
	}
	probes := roleProbes(models, func(key string) string { return env[key] })
	// gemini/K_RECEPTION once (implementer + assessor), deepseek/K_REVIEW once
	// (checker + review-a), the implementer agent, review A's agent key;
	// review B has no model in the environment and is skipped.
	if len(probes) != 4 {
		t.Fatalf("probes = %+v", probes)
	}
	if probes[0].Role != "受付 (対象の導出) / 受付 (検査 1 段目)" || probes[1].KeyEnv != "K_REVIEW" || probes[1].Role != "受付 (検査 2 段目) / レビュー役 (review-a)" ||
		probes[2].Model != "opus" || probes[3].KeyEnv != "LASSDAS_REVIEW_A_KEY" {
		t.Fatalf("probes = %+v", probes)
	}
}

func TestProbeBudgetOnlyABudgetRefusalCounts(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   bool
	}{
		"budget refusal": {http.StatusTooManyRequests, `{"error":{"message":"Monthly budget exceeded"}}`, true},
		"credit balance": {http.StatusTooManyRequests, `{"error":{"message":"Credit exhausted (current balance: 0)","type":"insufficient_quota","code":"credit_exhausted"}}`, true},
		"rate limit":     {http.StatusTooManyRequests, `{"error":{"message":"rate limit"}}`, false},
		"success":        {http.StatusOK, `{"choices":[]}`, false},
		"outage":         {http.StatusServiceUnavailable, `down`, false},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			var auth, path string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth, path = r.Header.Get("Authorization"), r.URL.Path
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			got, _, err := probeBudget(context.Background(), server.Client(), modelProbe{BaseURL: server.URL + "/", Model: "m", KeyEnv: "K"}, "secret")
			if err != nil || got != testCase.want || auth != "Bearer secret" || path != "/chat/completions" {
				t.Fatalf("probeBudget() = (%v, %v) auth=%q path=%q, want %v", got, err, auth, path, testCase.want)
			}
		})
	}
	if _, _, err := probeBudget(context.Background(), &http.Client{Timeout: time.Second}, modelProbe{BaseURL: "http://127.0.0.1:1", Model: "m"}, "k"); err == nil {
		t.Fatal("a transport failure must surface as an error, not a verdict")
	}
}

func TestCheckBudgetsHoldsOnceThrottlesAndClears(t *testing.T) {
	runDir := t.TempDir()
	var exhausted atomic.Bool
	exhausted.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if exhausted.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`Monthly budget exceeded`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()
	consumer := filepath.Join(t.TempDir(), "m1-consumer.json")
	raw, err := json.Marshal(map[string]any{"models": map[string]any{
		"implementer": map[string]any{"id": "r", "vendor": "Google", "model": "gemini", "base_url": server.URL, "api_key_env": "K_RECEPTION"},
		"reviewers":   []any{},
		"readiness": map[string]any{
			"assessor": map[string]any{"id": "a", "vendor": "Google", "model": "gemini", "base_url": server.URL, "api_key_env": "K_RECEPTION"},
			"checker":  map[string]any{"id": "c", "vendor": "DeepSeek", "model": "deepseek", "base_url": server.URL, "api_key_env": "K_REVIEW"},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(consumer, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	config := runtime.Config{ConsumerConfigPath: consumer}
	env := map[string]string{
		"K_RECEPTION": "k1", "K_REVIEW": "k2", "LASSDAS_IMPLEMENTER_KEY": "k3",
		"LASSDAS_GATEWAY_BASE_URL": server.URL, "LASSDAS_IMPLEMENTER_MODEL": "opus",
	}
	getenv := func(key string) string { return env[key] }
	run := state.RunOverview{RunID: "TKT-5", DeliveryID: "d5", IssueID: 50}
	source := &fakeConfirmationSource{}

	if !checkBudgets(context.Background(), config, source, run, runDir, 50, getenv, server.Client(), resolutionTestLogger{}) {
		t.Fatal("an exhausted key must hold the run")
	}
	if len(source.added) != 1 || hook.ExtractCommentMarker(source.added[0]) != hook.CommentMarker(string(hook.RunCommentBudgetHold), run.RunID) {
		t.Fatalf("notices = %d", len(source.added))
	}
	hold, ok := readBudgetHold(runDir)
	if !ok || len(hold.Roles) != 3 {
		t.Fatalf("hold = %+v (%v); want the three distinct keys", hold, ok)
	}
	if !budgetHeldRecently(runDir, time.Now()) || budgetHeldRecently(runDir, time.Now().Add(budgetRetryInterval+time.Second)) {
		t.Fatal("the hold must throttle the retry for exactly the interval")
	}

	// Still exhausted at the next attempt: held again, no second notice.
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 1, UserID: 1, Body: source.added[0]})
	if !checkBudgets(context.Background(), config, source, run, runDir, 50, getenv, server.Client(), resolutionTestLogger{}) || len(source.added) != 1 {
		t.Fatalf("second attempt: notices = %d", len(source.added))
	}

	// Budget restored: the hold clears and the run proceeds.
	exhausted.Store(false)
	if checkBudgets(context.Background(), config, source, run, runDir, 50, getenv, server.Client(), resolutionTestLogger{}) {
		t.Fatal("a key back in budget must not hold the run")
	}
	if _, ok := readBudgetHold(runDir); ok {
		t.Fatal("the hold file must be removed once the budget is back")
	}
}
