package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

func TestMetricValueReadsProbeExcerpts(t *testing.T) {
	cases := []struct {
		metric, excerpt string
		want            float64
		ok              bool
	}{
		{"time_total", "status=200 time_total=0.412 bytes=18422\n", 0.412, true},
		{"status", "status=302 time_total=0.1 bytes=0\n", 302, true},
		{"bytes", "status=200 time_total=0.1 bytes=18422 rotated=true\n", 18422, true},
		{"time_total", "status=200\n", 0, false},
		{"rows", "count\n42\n7\n", 2, true},
		{"rows", "", 0, false},
		{"value", "count\n42\n", 42, true},
		{"value", "count\n", 0, false},
		{"value", "count\nabc\n", 0, false},
		{"other", "x", 0, false},
	}
	for _, tc := range cases {
		got, ok := metricValue(tc.metric, tc.excerpt)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("metricValue(%q, %q) = %v %v, want %v %v", tc.metric, tc.excerpt, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMeasurementVerificationIsSkippedWithoutADesign(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	checked, _, _, err := p.measurementVerification(nil, t.TempDir(), "staging")
	if checked || err != nil {
		t.Errorf("no design: checked %v err %v", checked, err)
	}
}

func TestMeasurementCheckFilesLiveUnderTheWorkspace(t *testing.T) {
	p := &Pipeline{Workspace: t.TempDir()}
	if got := p.stageFile("history/stage-1", "staging-measurement-check.json"); got != p.path("history/stage-1/staging-measurement-check.json") {
		t.Errorf("relative stage dir resolved to %q", got)
	}
	abs := t.TempDir()
	if got := p.stageFile(abs, "x.json"); got != abs+"/x.json" {
		t.Errorf("absolute stage dir resolved to %q", got)
	}
}

func TestMeasurementVerificationRunsTheDesignsProbeEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	p := &Pipeline{Workspace: workspace}
	// A consumer config whose catalogue has one exec probe that prints a
	// timing line; the design promises time_total under 3 seconds.
	consumer := filepath.Join(t.TempDir(), "consumer.json")
	base, err := os.ReadFile("../../config/m1-consumer.json")
	if err != nil {
		t.Skip("example consumer config not available")
	}
	var config map[string]any
	if err := json.Unmarshal(base, &config); err != nil {
		t.Fatal(err)
	}
	config["probes"] = []map[string]any{{"id": "http.timing", "kind": "exec", "argv": []string{"echo", "status=200 time_total=0.412 bytes=1"}}}
	encoded, _ := json.Marshal(config)
	if err := os.WriteFile(consumer, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Config.ConsumerConfigPath = consumer
	writeDecision(t, workspace, `{"outcome":"ready","request_kind":"change","needs_design":true}`)
	identity := investigate.Identity{DeliveryID: "delivery-1", InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64), ToolSHA: "tool", BaseSHA: strings.Repeat("a", 40)}
	catalog, err := probe.NewCatalog([]probe.Spec{{ID: "http.timing", Kind: probe.KindExec, Argv: []string{"echo", "status=200 time_total=0.412 bytes=1"}}})
	if err != nil {
		t.Fatal(err)
	}
	measurements := p.path("measurements.jsonl")
	recorder, err := probe.OpenRecorder(measurements)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: t.TempDir()}
	if _, err := session.Run(context.Background(), probe.Request{Probe: "repo.list"}); err != nil {
		t.Fatal(err)
	}
	investigation, err := investigate.NewInvestigation(identity, 1, investigate.ModelInvestigationOutput{Questions: []string{"q"}, Next: "n",
		Findings: []investigate.Finding{{Claim: "c", Evidence: []string{"m-0001"}, Confidence: investigate.ConfidenceMeasured}}}, measurements, 1, investigate.Budget{ProbesUsed: 1})
	if err != nil {
		t.Fatal(err)
	}
	design, err := investigate.NewDesign(identity, 1, investigate.ModelDesignOutput{Cause: "c", CauseEvidence: []string{"m-0001"}, Approach: "a", Alternatives: []string{"x"},
		Files:        []investigate.FileChange{{Path: "web/a", Changes: []string{"x"}}},
		Verification: investigate.Verification{Form: investigate.VerificationMeasurement, Probe: "http.timing", Metric: "time_total", Threshold: 3},
		BlastRadius:  []string{"b"}}, investigation, investigate.Bounds{AllowedFilePrefixes: []string{"web/"}, MaxFiles: 4, Catalog: catalog, RepoRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.designRoundDir(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := investigate.Write(filepath.Join(p.designRoundDir(1), "investigation.json"), investigation); err != nil {
		t.Fatal(err)
	}
	if err := investigate.Write(filepath.Join(p.designRoundDir(1), "design.json"), design); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.designRoundDir(1), "decision.json"), []byte(`{"subject":"design","subject_sha256":"`+design.DesignSHA256+`","outcome":"approved"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stageDir := "history/stage-1"
	if err := os.MkdirAll(p.path(stageDir), 0o755); err != nil {
		t.Fatal(err)
	}
	checked, pass, detail, err := p.measurementVerification(context.Background(), stageDir, "staging")
	if err != nil || !checked || !pass {
		t.Fatalf("measurement: checked %v pass %v detail %q err %v", checked, pass, detail, err)
	}
	if _, err := os.Stat(p.path(stageDir + "/staging-measurement-check.json")); err != nil {
		t.Errorf("check file not under the workspace: %v", err)
	}
	report := DeliverReport{Phase: "staging", Verdict: "pass"}
	p.fillMeasurement(&report, stageDir, "staging")
	if report.Measurement == nil || !report.Measurement.Pass || report.Measurement.Value != 0.412 || report.Measurement.MeasurementID != "m-0001" {
		t.Errorf("report measurement: %+v", report.Measurement)
	}
}
