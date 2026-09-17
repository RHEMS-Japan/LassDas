package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

// A reception model failure leaves the worker's own detail beside the
// failed step: the phrase, the calls, the last answer's finish reason and
// reasoning tokens - what a live failure had to be read from the gateway's
// logs for.
func TestAReceptionModelFailureKeepsTheWorkersDetailBesideTheRun(t *testing.T) {
	detail, _ := json.Marshal(worker.ModelFailureDetail{
		Phrase: "model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); the whole allowance went to reasoning",
		Model:  "vendor/model-a", Effort: "medium", MaxOutputTokens: 32768, Calls: 3, Lowered: 2,
		LastRequestID: "gen-abc123", LastFinishReason: "length", LastPromptTokens: 900, LastCompletionTokens: 32768, LastReasoningTokens: 32768,
	})
	stderr := worker.FailureDetailLinePrefix + string(detail) + "\nworker: readiness assessment failed: model response ended before a complete answer: finish_reason=length (output allowance 32768 tokens); asked again with the wider allowance and cut off again"
	stub := receptionStubWorkerLines(t, "assess-readiness", stderr)
	pipeline := receptionPipeline(t, stub)
	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
	raw, readErr := os.ReadFile(pipeline.path(ModelFailureDetailFile))
	if readErr != nil {
		t.Fatalf("no detail record: %v", readErr)
	}
	var record struct {
		Step string `json:"step"`
		worker.ModelFailureDetail
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Step == "" || record.Model != "vendor/model-a" || record.Calls != 3 || record.LastReasoningTokens != 32768 || record.LastFinishReason != "length" || record.Lowered != 2 {
		t.Fatalf("record = %+v", record)
	}
	if info, err := os.Stat(pipeline.path(ModelFailureDetailFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode: %v %v", info, err)
	}
}

// A failure whose stderr carries no detail (or one outside the shape)
// leaves no record - and removes one an earlier attempt left.
func TestAReceptionFailureWithoutDetailLeavesNoRecord(t *testing.T) {
	stub := receptionStubWorkerLines(t, "assess-readiness", worker.FailureDetailLinePrefix+`{"phrase":"p","calls":1,"api_key":"sk-x"}`+"\nworker: readiness assessment failed: derived contract is invalid")
	pipeline := receptionPipeline(t, stub)
	if err := os.WriteFile(pipeline.path(ModelFailureDetailFile), []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pipeline.readinessGate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pipeline.path(ModelFailureDetailFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused detail must leave no record: %v", err)
	}
}

// receptionStubWorkerLines is receptionStubWorker for a multi-line stderr.
func receptionStubWorkerLines(t *testing.T, failing, stderr string) string {
	t.Helper()
	text := filepath.Join(t.TempDir(), "stderr.txt")
	if err := os.WriteFile(text, []byte(stderr+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\nif [ \"$1\" = \"" + failing + "\" ]; then cat '" + text + "' >&2; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// The billing reading the terminal comment carries is kept beside the run,
// per key with the roles it serves, so the ticket page shows a failed
// run's cost too.
func TestTheSpendReadingIsKeptBesideTheRun(t *testing.T) {
	t.Setenv("TEST_SPEND_IMPL_KEY", "impl-key-value")
	t.Setenv("TEST_SPEND_REVIEW_KEY", "review-key-value")
	// The same key under a second variable name: that is what "one key,
	// two seats" is, and it is what the record must fold.
	t.Setenv("TEST_SPEND_ASSESS_KEY", "impl-key-value")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/spend" || r.URL.Query().Get("since") == "" {
			http.NotFound(w, r)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer impl-key-value":
			_, _ = w.Write([]byte(`{"key_name":"automation-impl","spend_usd":0.54,"unpriced_requests":0}`))
		case "Bearer review-key-value":
			_, _ = w.Write([]byte(`{"key_name":"automation-review-a","spend_usd":2.23,"unpriced_requests":0}`))
		default:
			http.Error(w, "who", http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	config := worker.Config{Models: worker.ModelConfig{
		Implementer: worker.ModelEndpoint{Model: "vendor/model-a", BaseURL: server.URL, APIKeyEnv: "TEST_SPEND_IMPL_KEY"},
		Reviewers: []worker.ModelEndpoint{
			{ID: "review-a", Model: "vendor/model-b", BaseURL: server.URL, APIKeyEnv: "TEST_SPEND_REVIEW_KEY"},
			{ID: "review-b", Model: "vendor/model-a", BaseURL: server.URL, APIKeyEnv: "TEST_SPEND_IMPL_KEY"},
		},
		Readiness: worker.ReadinessModels{Assessor: worker.ModelEndpoint{Model: "vendor/model-a", BaseURL: server.URL, APIKeyEnv: "TEST_SPEND_ASSESS_KEY"}},
	}}
	workspace := t.TempDir()
	terminal := &Terminal{workspace: workspace, logger: trailTestLogger{}}
	reader, err := worker.NewGatewaySpendReader(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 15, 7, 5, 34, 0, time.UTC)
	text := terminal.readAndRecordSpend(context.Background(), config, reader, since)
	if !strings.Contains(text, "合計: $2.77") {
		t.Fatalf("spend text = %q", text)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, SpendRecordFile))
	if err != nil {
		t.Fatalf("no spend record: %v", err)
	}
	var record spendRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if !record.Complete || record.TotalUSD < 2.77-1e-9 || record.TotalUSD > 2.77+1e-9 || len(record.Keys) != 2 || record.Text != text || record.Since.IsZero() {
		t.Fatalf("record = %+v", record)
	}
	byName := map[string]spendRecordKey{}
	for _, key := range record.Keys {
		byName[key.KeyName] = key
	}
	// The implementer's key serves the implementer, review-b and (through
	// its own variable) the assessor: three roles on one line, one figure.
	if impl := byName["automation-impl"]; (impl.KeyEnv != "TEST_SPEND_IMPL_KEY" && impl.KeyEnv != "TEST_SPEND_ASSESS_KEY") || len(impl.Roles) < 3 || impl.SpendUSD != 0.54 {
		t.Fatalf("the implementer's key must carry every role it serves: %+v", impl)
	}
	if strings.Contains(string(raw), "impl-key-value") || strings.Contains(string(raw), "review-key-value") {
		t.Fatalf("a key value reached the record: %s", raw)
	}
	// No reading: the stale record goes, nothing replaces it.
	terminal.recordSpend(worker.RunSpend{}, nil, time.Now(), "")
	if _, err := os.Stat(filepath.Join(workspace, SpendRecordFile)); !os.IsNotExist(err) {
		t.Fatalf("an empty reading must leave no record: %v", err)
	}
}

// The intake (read-contract) is a model turn too: its failure leaves the
// detail like the readiness stages do. (The derivation already did.)
func TestAnIntakeModelFailureKeepsTheDetailToo(t *testing.T) {
	detail, _ := json.Marshal(worker.ModelFailureDetail{Phrase: "model invocation failed with status 429", Model: "vendor/model-a", Calls: 1, LastHTTPStatus: 429})
	stub := receptionStubWorkerLines(t, "read-contract", worker.FailureDetailLinePrefix+string(detail)+"\nworker: contract intake failed: model invocation failed with status 429 and no Retry-After (a limit that a wait does not lift)")
	pipeline := receptionPipeline(t, stub)
	if _, outcome, err := pipeline.pretrip(context.Background()); err == nil && outcome.Code != "internal_failed" {
		t.Fatalf("pretrip should fail on the intake: %+v", outcome)
	}
	raw, err := os.ReadFile(pipeline.path(ModelFailureDetailFile))
	if err != nil {
		t.Fatalf("no detail record for the intake: %v", err)
	}
	if !strings.Contains(string(raw), `"step":"依頼の読み取り"`) || !strings.Contains(string(raw), `"last_http_status":429`) {
		t.Fatalf("record = %s", raw)
	}
	if _, err := os.Stat(pipeline.path(ModelFailureDetailFile) + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temp file must not remain")
	}
}

// Two variables of one key that serve the same role name once: the record
// lists the role once, as the comment does.
func TestSpendRolesAreListedOnce(t *testing.T) {
	terminal := &Terminal{workspace: t.TempDir(), logger: trailTestLogger{}}
	spend := worker.RunSpend{Complete: true, TotalUSD: 1, Keys: []worker.KeySpend{{KeyEnv: "A", KeyName: "k", SpendUSD: 1, AlsoKeyEnvs: []string{"B"}}}}
	terminal.recordSpend(spend, map[string][]string{"A": {"受付"}, "B": {"受付"}}, time.Now(), "t")
	raw, err := os.ReadFile(filepath.Join(terminal.workspace, SpendRecordFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"roles":["受付"]`) {
		t.Fatalf("roles must be listed once: %s", raw)
	}
}
