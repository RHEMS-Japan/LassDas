package worker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

func sealedDesign(t *testing.T, files ...string) (investigate.Design, investigate.Identity) {
	t.Helper()
	identity := investigate.Identity{DeliveryID: "delivery-1", InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64), ToolSHA: "tool", BaseSHA: strings.Repeat("a", 40)}
	catalog, _ := probe.NewCatalog(nil)
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := probe.OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: t.TempDir()}
	if _, err := session.Run(context.Background(), probe.Request{Probe: "repo.list"}); err != nil {
		t.Fatal(err)
	}
	investigation, err := investigate.NewInvestigation(identity, 1, investigate.ModelInvestigationOutput{Questions: []string{"q"}, Next: "n",
		Findings: []investigate.Finding{{Claim: "c", Evidence: []string{"m-0001"}, Confidence: investigate.ConfidenceMeasured}}}, path, 1, investigate.Budget{ProbesUsed: 1})
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]investigate.FileChange, 0, len(files))
	for _, file := range files {
		changes = append(changes, investigate.FileChange{Path: file, Changes: []string{"x"}})
	}
	design, err := investigate.NewDesign(identity, 1, investigate.ModelDesignOutput{Cause: "c", CauseEvidence: []string{"m-0001"}, Approach: "a", Files: changes,
		Verification: investigate.Verification{Form: investigate.VerificationWording, Path: "/p", ExpectedText: "new"}, BlastRadius: []string{"b"}},
		investigation, investigate.Bounds{AllowedFilePrefixes: []string{"web/"}, MaxFiles: 4, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	return design, identity
}

func TestPublishGateRequiresDesignSubset(t *testing.T) {
	design, identity := sealedDesign(t, "web/a.ts", "web/b.ts")
	candidate := Candidate{DeliveryID: identity.DeliveryID, InputSHA256: identity.InputSHA256, ConfigSHA256: identity.ConfigSHA256, ToolSHA: identity.ToolSHA, BaseSHA: identity.BaseSHA,
		DesignSHA256: design.DesignSHA256, Files: []CandidateFile{{Path: "web/a.ts"}}}
	approved := DesignDecisionSummary{Subject: "design", SubjectSHA256: design.DesignSHA256, Outcome: "approved"}
	if err := ValidateDesignBinding(candidate, design, approved); err != nil {
		t.Fatalf("subset candidate refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Candidate, *investigate.Design, *DesignDecisionSummary)
		reason string
	}{
		{"file outside the design", func(c *Candidate, _ *investigate.Design, _ *DesignDecisionSummary) {
			c.Files = append(c.Files, CandidateFile{Path: "web/c.ts"})
		}, "outside the design"},
		{"no design fingerprint", func(c *Candidate, _ *investigate.Design, _ *DesignDecisionSummary) { c.DesignSHA256 = "" }, "no design fingerprint"},
		{"other design", func(c *Candidate, _ *investigate.Design, _ *DesignDecisionSummary) {
			c.DesignSHA256 = strings.Repeat("f", 64)
		}, "do not agree"},
		{"tampered design", func(_ *Candidate, d *investigate.Design, _ *DesignDecisionSummary) { d.Approach = "something else" }, "do not agree"},
		{"decision on another record", func(_ *Candidate, _ *investigate.Design, s *DesignDecisionSummary) {
			s.SubjectSHA256 = strings.Repeat("e", 64)
		}, "did not approve"},
		{"decision revise", func(_ *Candidate, _ *investigate.Design, s *DesignDecisionSummary) { s.Outcome = "revise" }, "did not approve"},
		{"decision about the investigation", func(_ *Candidate, _ *investigate.Design, s *DesignDecisionSummary) { s.Subject = "investigation" }, "did not approve"},
		{"another run", func(c *Candidate, _ *investigate.Design, _ *DesignDecisionSummary) { c.DeliveryID = "delivery-2" }, "another run"},
	}
	for _, tc := range cases {
		c, d, s := candidate, design, approved
		c.Files = append([]CandidateFile(nil), candidate.Files...)
		tc.mutate(&c, &d, &s)
		if err := ValidateDesignBinding(c, d, s); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	if err := ChangedFilesWithinDesign([]string{"web/a.ts", "web/b.ts"}, design); err != nil {
		t.Errorf("exact set refused: %v", err)
	}
	if err := ChangedFilesWithinDesign([]string{"web/a.ts", "docs/x.md"}, design); err == nil || !strings.Contains(err.Error(), "docs/x.md") {
		t.Errorf("outside file not named: %v", err)
	}
}
