package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// sealedDesignFor writes a sealed design for the fixture's run that names
// the given files, and returns its path.
func sealedDesignFor(t *testing.T, fixture agentFixture, files ...string) string {
	return sealedDesignForRun(t, fixture, "", files...)
}

// sealedDesignForRun is sealedDesignFor with the delivery id overridden, to
// stand for another run's design.
func sealedDesignForRun(t *testing.T, fixture agentFixture, deliveryID string, files ...string) string {
	t.Helper()
	var draft worker.TicketDraft
	readAgentArtifact(t, fixture.draftPath, worker.MaxTicketJSONBytes, &draft)
	if deliveryID == "" {
		deliveryID = draft.DeliveryID
	}
	identity := investigate.Identity{DeliveryID: deliveryID, InputSHA256: draft.InputSHA256, ConfigSHA256: draft.ConfigSHA256, ToolSHA: draft.ToolSHA, BaseSHA: fixture.baseSHA}
	catalog, err := probe.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	measurements := fixture.path("measurements.jsonl")
	recorder, err := probe.OpenRecorder(measurements)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: fixture.repoRoot}
	if _, err := session.Run(context.Background(), probe.Request{Probe: "repo.list"}); err != nil {
		t.Fatal(err)
	}
	investigation, err := investigate.NewInvestigation(identity, 1, investigate.ModelInvestigationOutput{
		Questions: []string{"Where is the label?"},
		Findings:  []investigate.Finding{{Claim: "The label lives in the client", Evidence: []string{"m-0001"}, Confidence: investigate.ConfidenceMeasured}},
		Next:      "Replace it.",
	}, measurements, 1, investigate.Budget{ProbesUsed: 1, ElapsedSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := fixture.config.ConsumerFor(draft.Repository)
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]investigate.FileChange, 0, len(files))
	for _, file := range files {
		changes = append(changes, investigate.FileChange{Path: file, Changes: []string{"update the label"}})
	}
	design, err := investigate.NewDesign(identity, 1, investigate.ModelDesignOutput{
		Cause: "The label is hard-coded", CauseEvidence: []string{"m-0001"}, Approach: "Replace the label",
		Files: changes, Verification: investigate.Verification{Form: investigate.VerificationWording, Path: "/page", ExpectedText: "Updated label", AbsentText: "Old label"},
		BlastRadius: []string{"the page"},
	}, investigation, investigate.Bounds{AllowedFilePrefixes: consumer.Mode.AllowedFilePrefixes, MaxFiles: consumer.Mode.MaxFiles, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	dir := fixture.path("history/design-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := investigate.Write(filepath.Join(dir, "design.json"), design); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "design.json")
}

func TestSealRefusesFilesOutsideDesign(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	design := sealedDesignFor(t, fixture, "client/src/other.ts")
	if err := os.WriteFile(filepath.Join(fixture.repoRoot, "client", "src", "label.ts"), []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := fixture.sealCandidate(t, "--design", design)
	if err == nil || !strings.Contains(err.Error(), "outside the design") {
		t.Fatalf("a change outside the design was sealed: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("candidate.json")); statErr == nil {
		t.Fatal("a candidate was written for a refused change")
	}
	// The same change against a design that names the file seals, and the
	// candidate carries the design's fingerprint.
	fixture2 := newAgentFixture(t, "true", "true")
	design2 := sealedDesignFor(t, fixture2, "client/src/label.ts")
	if err := os.WriteFile(filepath.Join(fixture2.repoRoot, "client", "src", "label.ts"), []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture2.sealCandidate(t, "--design", design2); err != nil {
		t.Fatalf("a change inside the design was refused: %v", err)
	}
	var candidate worker.Candidate
	readAgentArtifact(t, fixture2.path("candidate.json"), worker.MaxArtifactJSONBytes, &candidate)
	loaded, err := investigate.ReadDesign(design2)
	if err != nil || candidate.DesignSHA256 != loaded.DesignSHA256 {
		t.Fatalf("candidate design fingerprint %q, want %q (%v)", candidate.DesignSHA256, loaded.DesignSHA256, err)
	}
	var source worker.SourceSnapshot
	var request worker.TicketRequest
	readAgentArtifact(t, fixture2.path("source.json"), worker.MaxArtifactJSONBytes, &source)
	readAgentArtifact(t, fixture2.path("ticket.json"), worker.MaxTicketJSONBytes, &request)
	if err := candidate.Validate(source, request, fixture2.config); err != nil {
		t.Fatalf("design-backed candidate rejected by its own validation: %v", err)
	}
	// A design of another run is refused before anything is sealed.
	fixture3 := newAgentFixture(t, "true", "true")
	foreign := sealedDesignForRun(t, fixture3, "another-delivery", "client/src/label.ts")
	if err := os.WriteFile(filepath.Join(fixture3.repoRoot, "client", "src", "label.ts"), []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture3.sealCandidate(t, "--design", foreign); err == nil || !strings.Contains(err.Error(), "another run") {
		t.Fatalf("another run's design accepted: %v", err)
	}
}

func TestSealTurnsObjectionIntoDesignRound(t *testing.T) {
	fixture := newAgentFixture(t, "true", "true")
	design := sealedDesignFor(t, fixture, "client/src/label.ts")
	objection := fixture.path("revise-design.json")
	if err := os.WriteFile(objection, []byte(`{"reason":"the label is rendered from a translation key, not the file the design names","section":"files"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stageDir := fixture.path("history/stage-1")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	err := fixture.sealCandidate(t, "--design", design, "--objection", objection, "--objection-out", filepath.Join(stageDir, "design-objection.json"))
	if err == nil || !strings.Contains(err.Error(), "objected") {
		t.Fatalf("objection did not fail the seal: %v", err)
	}
	if _, statErr := os.Stat(fixture.path("candidate.json")); statErr == nil {
		t.Fatal("a candidate was sealed despite the objection")
	}
	raw, readErr := os.ReadFile(filepath.Join(stageDir, "design-objection.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	var record DesignObjection
	if err := json.Unmarshal(raw, &record); err != nil || record.Section != "files" || !strings.Contains(record.Reason, "translation key") || record.ObjectionSHA256 == "" || record.DesignSHA256 == "" {
		t.Fatalf("objection record: %+v (%v)", record, err)
	}
	if _, statErr := os.Stat(objection); statErr == nil {
		t.Fatal("the applier's objection file stayed in the working directory; the next seal would find it again")
	}
	if _, statErr := os.Stat(filepath.Join(stageDir, "revise-design.json")); statErr != nil {
		t.Fatal("the applier's objection file was not moved into the round")
	}
	// A second seal of the same round finds no objection and seals normally.
	if err := os.WriteFile(filepath.Join(fixture.repoRoot, "client", "src", "label.ts"), []byte("export const label = 'Updated label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture.sealCandidate(t, "--design", design, "--objection", objection, "--objection-out", filepath.Join(stageDir, "design-objection-2.json")); err != nil {
		t.Fatalf("second seal after the objection: %v", err)
	}
	// An unreadable objection is refused rather than silently ignored.
	fixture2 := newAgentFixture(t, "true", "true")
	design2 := sealedDesignFor(t, fixture2, "client/src/label.ts")
	if err := os.WriteFile(fixture2.path("revise-design.json"), []byte(`{"reason":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fixture2.sealCandidate(t, "--design", design2, "--objection", fixture2.path("revise-design.json"), "--objection-out", fixture2.path("design-objection.json")); err == nil || !strings.Contains(err.Error(), "readable reason") {
		t.Fatalf("empty objection accepted: %v", err)
	}
}
