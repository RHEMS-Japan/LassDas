package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
	"errors"
	"regexp"
)

// The run records what each key had been billed before it spent anything,
// and the report finds it again. Without the record a provider that cannot
// bill per window leaves the requester no cost line at all.
func TestTheRunRecordsWhatEachKeyHadBeenBilledBeforeItStarted(t *testing.T) {
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"usage":2.5}}`)
	}))
	defer server.Close()
	t.Setenv("SPEND_KEY", "a-key")

	pipeline := &Pipeline{Workspace: workspace}
	pipeline.writeSpendBaseline(context.Background(), oneSeatConfig(server.URL, "SPEND_KEY"), usageReaderFor(t, server))

	baseline, found := loadSpendBaseline(workspace)
	if !found {
		raw, _ := os.ReadFile(filepath.Join(workspace, SpendBaselineFile))
		t.Fatalf("no baseline was recorded: %q", raw)
	}
	if len(baseline.Keys) != 1 || baseline.Keys[0].KeyEnv != "SPEND_KEY" || baseline.Keys[0].UsageUSD != 2.5 {
		t.Fatalf("baseline = %+v", baseline)
	}
	if baseline.TakenAt.IsZero() {
		t.Error("the baseline does not say when it was taken")
	}
}

// A gateway that cannot answer leaves no record, and the run starts anyway:
// a billing figure is a line in a report, never a gate on the work.
func TestAGatewayThatCannotAnswerLeavesNoBaselineAndStopsNothing(t *testing.T) {
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such endpoint", http.StatusNotFound)
	}))
	defer server.Close()
	t.Setenv("SPEND_KEY", "a-key")

	pipeline := &Pipeline{Workspace: workspace}
	pipeline.writeSpendBaseline(context.Background(), oneSeatConfig(server.URL, "SPEND_KEY"), usageReaderFor(t, server))

	if _, found := loadSpendBaseline(workspace); found {
		t.Fatal("a baseline was recorded from a gateway that answered nothing")
	}
}

// With a baseline beside the run, the report falls back to the difference
// when the per-window endpoint is not there - which is the whole point: the
// live deployment talks to a provider that has no such endpoint.
func TestTheReportFallsBackToTheDifferenceWhenThereIsNoBillingWindow(t *testing.T) {
	workspace := t.TempDir()
	var windowAsked, keyAsked int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key/spend":
			windowAsked++
			http.Error(w, "not found", http.StatusNotFound)
		case "/key":
			keyAsked++
			_, _ = io.WriteString(w, `{"data":{"usage":4.0}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("SPEND_KEY", "a-key")

	encoded, err := json.Marshal(worker.UsageBaseline{
		TakenAt: time.Now().UTC().Add(-time.Hour),
		Keys:    []worker.KeyUsage{{KeyEnv: "SPEND_KEY", UsageUSD: 3.0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, SpendBaselineFile), encoded, 0o600); err != nil {
		t.Fatal(err)
	}

	terminal := &Terminal{workspace: workspace, logger: &baselineTestLogger{}}
	text := terminal.readSpendWith(context.Background(), oneSeatConfig(server.URL, "SPEND_KEY"),
		server.Client(), time.Now().Add(-time.Hour))

	if windowAsked == 0 {
		t.Error("the precise reading was never asked for")
	}
	if keyAsked == 0 {
		t.Error("the difference was never taken")
	}
	// $4.00 now less $3.00 at the start.
	if !strings.Contains(text, "$1.00") {
		t.Fatalf("the cost line does not carry the difference: %q", text)
	}
	if !strings.Contains(text, "差です") {
		t.Errorf("the requester was not told the figure is a difference: %q", text)
	}
	raw, err := os.ReadFile(filepath.Join(workspace, SpendRecordFile))
	if err != nil {
		t.Fatalf("no spend record was kept: %v", err)
	}
	var record struct {
		Approximate bool    `json:"approximate"`
		TotalUSD    float64 `json:"total_usd"`
	}
	if json.Unmarshal(raw, &record) != nil || !record.Approximate || record.TotalUSD != 1.0 {
		t.Fatalf("the record does not say what kind of number it holds: %s", raw)
	}
}

// oneSeatConfig bills one gateway through one key variable.
func oneSeatConfig(baseURL, keyEnv string) worker.Config {
	config := worker.Config{}
	config.Models.Implementer = worker.ModelEndpoint{BaseURL: baseURL, APIKeyEnv: keyEnv, Model: "m"}
	config.Models.Reviewers = []worker.ModelEndpoint{{ID: "review-a", BaseURL: baseURL, APIKeyEnv: keyEnv, Model: "m"}}
	return config
}

func usageReaderFor(t *testing.T, server *httptest.Server) worker.UsageReader {
	t.Helper()
	reader, err := worker.NewGatewayUsageReader(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

type baselineTestLogger struct{}

func (baselineTestLogger) Info(string, ...any)  {}
func (baselineTestLogger) Error(string, ...any) {}

// The baseline is worthless taken late: every call the run has already paid
// for would be inside it. This reads the reception's own source and fails if
// the reading is gone or has moved behind a model call - the wiring is the
// whole feature, and a test that exercises the parts without it passes while
// nothing happens in production (review of #199).
func TestTheBaselineIsTakenBeforeTheRunPaysForAnything(t *testing.T) {
	raw, err := os.ReadFile("stages.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	start := strings.Index(body, "func (p *Pipeline) pretrip(")
	if start < 0 {
		t.Fatal("the reception's function was not found; this check is looking in the wrong place")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("the reception's function does not end")
	}
	reception := body[start : start+end]
	baselineAt := strings.Index(reception, "recordSpendBaseline(")
	if baselineAt < 0 {
		t.Fatal("the reception never reads what the keys had been billed, so no run can report a cost")
	}
	// Prepare clears the workspace. A reading taken before it is written
	// and then deleted seconds later, which looks like the feature working
	// and is the feature doing nothing (review of #202).
	clearedAt := strings.Index(reception, "p.Prepare()")
	if clearedAt < 0 {
		t.Fatal("the reception no longer clears the workspace; this check is looking in the wrong place")
	}
	if baselineAt < clearedAt {
		t.Fatal("the reading is taken before the workspace is cleared, so the record it writes is deleted")
	}
	firstPaidCall := strings.Index(reception, "p.worker(ctx,")
	if firstPaidCall < 0 {
		t.Fatal("the reception starts no model step; this check is looking in the wrong place")
	}
	if baselineAt > firstPaidCall {
		t.Fatal("the reading is taken after the run has already paid for something, so that spend is inside it")
	}
}

// The entry point production uses, not only the helper underneath it: it
// reads the destination's configuration, builds its own client, and writes
// the record. Tested through a stand-in transport, because a real
// configuration may only name an https endpoint with no port.
func TestTheReceptionsOwnBaselineReadingWritesTheRecord(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("SPEND_KEY", "a-key")
	var asked int
	pipeline := &Pipeline{
		Workspace: workspace,
		Config:    runtime.Config{ConsumerConfigPath: consumerConfigFile(t, "https://gateway.example", "SPEND_KEY")},
		usageTransport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/key" {
				return nil, errors.New("asked for " + r.URL.Path)
			}
			asked++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"usage":8.25}}`)), Header: http.Header{}}, nil
		}),
	}
	pipeline.recordSpendBaseline(context.Background())

	if asked == 0 {
		t.Fatal("the reception never asked what the keys had been billed")
	}
	baseline, found := loadSpendBaseline(workspace)
	if !found {
		t.Fatal("the reception asked and wrote nothing")
	}
	if len(baseline.Keys) == 0 || baseline.Keys[0].UsageUSD != 8.25 {
		t.Fatalf("baseline = %+v", baseline)
	}
}

// A destination whose configuration cannot be read leaves no record, and
// the run starts anyway.
func TestAnUnreadableConfigurationLeavesNoBaseline(t *testing.T) {
	workspace := t.TempDir()
	pipeline := &Pipeline{Workspace: workspace, Config: runtime.Config{
		ConsumerConfigPath: filepath.Join(t.TempDir(), "absent.json"),
	}}
	pipeline.recordSpendBaseline(context.Background())
	if _, found := loadSpendBaseline(workspace); found {
		t.Fatal("a baseline was written without a configuration to read")
	}
}

// consumerConfigFile is the shipped example destination configuration with
// its model endpoints pointed at one gateway and one key variable. Built
// from the real file so the fixture cannot drift from what the loader
// requires; the URL stays a legal one, and the transport stands in.
func consumerConfigFile(t *testing.T, baseURL, keyEnv string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "m1-consumer.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`"base_url"\s*:\s*"[^"]*"`).ReplaceAllString(string(raw), `"base_url": "`+baseURL+`"`)
	body = regexp.MustCompile(`"api_key_env"\s*:\s*"[^"]*"`).ReplaceAllString(body, `"api_key_env": "`+keyEnv+`"`)
	path := filepath.Join(t.TempDir(), "consumer.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A run that dies during intake has already paid for the call that would
// have written the intake record. Its cost was reported as nothing at all,
// because the window was read from that record (review of #202).
func TestARunThatDiesInIntakeStillReportsWhatItSpent(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("SPEND_KEY", "a-key")
	if err := os.WriteFile(filepath.Join(workspace, SpendBaselineFile), mustJSON(t, worker.UsageBaseline{
		TakenAt: time.Now().UTC().Add(-10 * time.Minute),
		Keys:    []worker.KeyUsage{{KeyEnv: "SPEND_KEY", UsageUSD: 6.0}},
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	// No intake.json: the call that writes it is the one that died.
	if _, err := os.Stat(filepath.Join(workspace, "intake.json")); err == nil {
		t.Fatal("the fixture has an intake record")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			_, _ = io.WriteString(w, `{"data":{"usage":9.0}}`)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer server.Close()

	terminal := &Terminal{
		workspace: workspace,
		config:    runtime.Config{ConsumerConfigPath: consumerConfigFile(t, "https://gateway.example", "SPEND_KEY")},
		logger:    &baselineTestLogger{},
	}
	text := terminal.readSpendWith(context.Background(), oneSeatConfig(server.URL, "SPEND_KEY"),
		server.Client(), terminal.spendWindowStart())
	if !strings.Contains(text, "$3.00") {
		t.Fatalf("the requester was told nothing about what the run spent: %q", text)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
