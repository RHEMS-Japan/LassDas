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

	"automation.internal/ticket-ingress/internal/worker"
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
	firstPaidCall := strings.Index(reception, "p.worker(ctx,")
	if firstPaidCall < 0 {
		t.Fatal("the reception starts no model step; this check is looking in the wrong place")
	}
	if baselineAt > firstPaidCall {
		t.Fatal("the reading is taken after the run has already paid for something, so that spend is inside it")
	}
}
