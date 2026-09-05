package investigate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var reviewedAt = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func testReviewer(id string) Reviewer {
	return Reviewer{ID: id, Vendor: "vendor-" + id, Model: "model-" + id, BaseURL: "https://gateway.example.invalid/api/v1", Lens: "evidence"}
}

func testUsage(id string) Usage {
	return Usage{RequestedModel: "model-" + id, RequestID: "run-" + id, StopReason: "stop", InputTokens: 10, OutputTokens: 5, TotalTokens: 15, LatencyMillis: 20}
}

// designSubjectFixture seals an investigation and a design over the shared
// measurements fixture, the two records a design review may judge.
func designSubjectFixture(t *testing.T) (Investigation, Design) {
	t.Helper()
	path, _ := measurementsFile(t)
	investigation := goodInvestigation(t, path)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "page.tmpl"), []byte("<h1>Old label</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	design, err := NewDesign(testIdentity, 1, goodDesignOutput(), investigation, testBounds(t, root))
	if err != nil {
		t.Fatal(err)
	}
	return investigation, design
}

func passOutput() ModelDesignReviewOutput {
	return ModelDesignReviewOutput{Verdict: VerdictPass, Findings: []DesignFinding{}}
}

func reviseOutput(section string) ModelDesignReviewOutput {
	return ModelDesignReviewOutput{Verdict: VerdictRevise, Findings: []DesignFinding{{
		Code: "unmeasured-cache", Section: section,
		Message: "The cause cites one read of the template; whether the page caches the text was never measured.",
	}}}
}

func sealedReview(t *testing.T, subject ReviewSubject, reviewerID string, output ModelDesignReviewOutput, usage Usage) DesignReview {
	t.Helper()
	review, err := NewDesignReview(testIdentity, subject, testReviewer(reviewerID), output, usage, reviewedAt)
	if err != nil {
		t.Fatalf("review by %s refused: %v", reviewerID, err)
	}
	return review
}

func TestDesignReviewArtifacts(t *testing.T) {
	investigation, design := designSubjectFixture(t)
	subject := DesignSubject(design)
	pass := sealedReview(t, subject, "review-a", passOutput(), testUsage("review-a"))
	if pass.Subject != SubjectDesign || pass.SubjectSHA256 != design.DesignSHA256 || pass.Round != 1 || pass.ReviewSHA256 == "" || len(pass.Findings) != 0 {
		t.Fatalf("sealed review: %+v", pass)
	}
	if err := pass.Validate(testIdentity, subject); err != nil {
		t.Fatalf("re-validation: %v", err)
	}
	revise := sealedReview(t, subject, "review-b", reviseOutput(SectionCause), testUsage("review-b"))
	if revise.Verdict != VerdictRevise || len(revise.Findings) != 1 || revise.Findings[0].Section != SectionCause {
		t.Fatalf("sealed revise: %+v", revise)
	}

	// The verdict rules and the section rules hold at sealing time.
	many := make([]DesignFinding, 0, maxDesignFindings+1)
	for len(many) <= maxDesignFindings {
		many = append(many, DesignFinding{Code: "bulk", Section: SectionFiles, Message: "one more"})
	}
	malformedCode := reviseOutput(SectionCause)
	malformedCode.Findings[0].Code = "Bad Code"
	blankMessage := reviseOutput(SectionCause)
	blankMessage.Findings[0].Message = "  "
	refused := []struct {
		name   string
		output ModelDesignReviewOutput
		reason string
	}{
		{"pass with findings", ModelDesignReviewOutput{Verdict: VerdictPass, Findings: reviseOutput(SectionCause).Findings}, "no findings"},
		{"revise without findings", ModelDesignReviewOutput{Verdict: VerdictRevise}, "at least one"},
		{"unknown verdict", ModelDesignReviewOutput{Verdict: "approve"}, "pass or revise"},
		{"report section on a design", reviseOutput(SectionFindings), "not a section"},
		{"unknown section", reviseOutput("style"), "not a section"},
		{"malformed code", malformedCode, "code"},
		{"blank message", blankMessage, "message"},
		{"too many findings", ModelDesignReviewOutput{Verdict: VerdictRevise, Findings: many}, "too many"},
	}
	for _, tc := range refused {
		if _, err := NewDesignReview(testIdentity, subject, testReviewer("review-a"), tc.output, testUsage("review-a"), reviewedAt); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	// The invocation evidence, the clock and the reviewer are held like any review's.
	if _, err := NewDesignReview(testIdentity, subject, testReviewer("review-a"), passOutput(), testUsage("review-b"), reviewedAt); err == nil {
		t.Error("usage recorded for another model was accepted")
	}
	if _, err := NewDesignReview(testIdentity, subject, testReviewer("review-a"), passOutput(), testUsage("review-a"), reviewedAt.In(time.FixedZone("elsewhere", 3600))); err == nil {
		t.Error("a review time outside UTC was accepted")
	}
	if _, err := NewDesignReview(testIdentity, subject, Reviewer{ID: "Review A"}, passOutput(), testUsage("review-a"), reviewedAt); err == nil {
		t.Error("a malformed reviewer was accepted")
	}

	// An investigation report is judged in its own sections.
	reportSubject := InvestigationSubject(investigation)
	report := sealedReview(t, reportSubject, "review-a", reviseOutput(SectionUnknowns), testUsage("review-a"))
	if report.Subject != SubjectInvestigation || report.SubjectSHA256 != investigation.InvestigationSHA256 {
		t.Fatalf("report review: %+v", report)
	}
	if _, err := NewDesignReview(testIdentity, reportSubject, testReviewer("review-a"), reviseOutput(SectionCause), testUsage("review-a"), reviewedAt); err == nil || !strings.Contains(err.Error(), "not a section") {
		t.Errorf("design section on a report: err = %v", err)
	}

	// A review of one record is not a review of another record, round or delivery.
	if err := report.Validate(testIdentity, subject); err == nil {
		t.Error("a report review validated as a design review")
	}
	if err := pass.Validate(testIdentity, reportSubject); err == nil {
		t.Error("a design review validated as a report review")
	}
	otherRound := subject
	otherRound.Round = 2
	if err := pass.Validate(testIdentity, otherRound); err == nil {
		t.Error("a review validated against another round")
	}
	if err := pass.Validate(Identity{DeliveryID: "other"}, subject); err == nil {
		t.Error("a review validated under another identity")
	}
	resubject := pass
	resubject.SubjectSHA256 = strings.Repeat("f", 64)
	if err := resubject.Validate(testIdentity, subject); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Errorf("re-pointed review: %v", err)
	}

	// Tampering with the sealed text or verdict is caught.
	tampered := revise
	tampered.Findings = append([]DesignFinding(nil), revise.Findings...)
	tampered.Findings[0].Message = "Looks fine after all."
	if err := tampered.Validate(testIdentity, subject); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("tampered message: %v", err)
	}
	flipped := revise
	flipped.Verdict = VerdictPass
	if err := flipped.Validate(testIdentity, subject); err == nil {
		t.Error("a flipped verdict was accepted")
	}

	// The record survives the disk and the reviewer's answer is read strictly.
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "design-review-a.json"), pass); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDesignReview(filepath.Join(dir, "design-review-a.json"))
	if err != nil || read.Validate(testIdentity, subject) != nil || read.ReviewSHA256 != pass.ReviewSHA256 {
		t.Errorf("round trip: %v", err)
	}
	if _, err := DecodeModelDesignReviewOutput([]byte(`{"verdict":"revise","findings":[{"code":"x-y","section":"cause","message":"m","path":"web/page.tmpl"}]}`)); err == nil {
		t.Error("a finding carrying a path was accepted")
	}
	if _, err := DecodeModelDesignReviewOutput([]byte(`{"verdict":"pass","findings":[]} trailing`)); err == nil {
		t.Error("trailing content was accepted")
	}
}

func TestDecideDesignStopsAtTheRoundLimit(t *testing.T) {
	investigation, design := designSubjectFixture(t)
	subject := DesignSubject(design)
	passA := sealedReview(t, subject, "review-a", passOutput(), testUsage("review-a"))
	passB := sealedReview(t, subject, "review-b", passOutput(), testUsage("review-b"))
	reviseB := sealedReview(t, subject, "review-b", reviseOutput(SectionFiles), testUsage("review-b"))

	approved, err := DecideDesign(testIdentity, subject, []DesignReview{passB, passA}, 1, 3)
	if err != nil {
		t.Fatalf("approved decision refused: %v", err)
	}
	if approved.Outcome != OutcomeApproved || approved.SubjectSHA256 != design.DesignSHA256 || len(approved.ReviewSHA256s) != 2 ||
		approved.ReviewSHA256s[0] != passA.ReviewSHA256 || approved.ReviewSHA256s[1] != passB.ReviewSHA256 {
		t.Fatalf("approved decision: %+v", approved)
	}
	if err := approved.Validate(testIdentity, subject, []DesignReview{passA, passB}, 3); err != nil {
		t.Fatalf("re-validation: %v", err)
	}
	// One veto sends the design back while rounds remain...
	revise, err := DecideDesign(testIdentity, subject, []DesignReview{passA, reviseB}, 1, 3)
	if err != nil || revise.Outcome != OutcomeRevise {
		t.Fatalf("revise decision: %+v %v", revise, err)
	}
	// ...and ends the delivery honestly at the last round.
	last, err := DecideDesign(testIdentity, subject, []DesignReview{passA, reviseB}, 1, 1)
	if err != nil || last.Outcome != OutcomeNonconverged {
		t.Fatalf("last-round decision: %+v %v", last, err)
	}
	if err := last.Validate(testIdentity, subject, []DesignReview{passA, reviseB}, 1); err != nil {
		t.Errorf("last-round re-validation: %v", err)
	}
	// The limit is part of the derivation: read under a longer limit the same decision no longer holds.
	if err := last.Validate(testIdentity, subject, []DesignReview{passA, reviseB}, 3); err == nil {
		t.Error("a nonconverged decision validated under a longer limit")
	}

	// Refusals.
	againA := sealedReview(t, subject, "review-a", passOutput(), Usage{RequestedModel: "model-review-a", RequestID: "run-review-a-again", StopReason: "stop", InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	replayedB := sealedReview(t, subject, "review-b", passOutput(), Usage{RequestedModel: "model-review-b", RequestID: "run-review-a", StopReason: "stop", InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	reportSubject := InvestigationSubject(investigation)
	reportPass := sealedReview(t, reportSubject, "review-a", passOutput(), testUsage("review-a"))
	refused := []struct {
		name    string
		reviews []DesignReview
		round   int
		max     int
	}{
		{"one review of a design", []DesignReview{passA}, 1, 3},
		{"the same reviewer twice", []DesignReview{passA, againA}, 1, 3},
		{"the same invocation twice", []DesignReview{passA, replayedB}, 1, 3},
		{"no rounds at all", []DesignReview{passA, passB}, 1, 0},
		{"a round past the limit", []DesignReview{passA, passB}, 1, 0},
		{"a round that is not the subject's", []DesignReview{passA, passB}, 2, 3},
		{"a review of another subject", []DesignReview{passA, reportPass}, 1, 3},
	}
	for _, tc := range refused {
		if _, err := DecideDesign(testIdentity, subject, tc.reviews, tc.round, tc.max); err == nil {
			t.Errorf("%s: decision sealed", tc.name)
		}
	}

	// An investigation report takes the evidence review alone.
	decision, err := DecideDesign(testIdentity, reportSubject, []DesignReview{reportPass}, 1, 3)
	if err != nil || decision.Outcome != OutcomeApproved || len(decision.ReviewSHA256s) != 1 {
		t.Fatalf("report decision: %+v %v", decision, err)
	}
	reportRevise := sealedReview(t, reportSubject, "review-a", reviseOutput(SectionUnknowns), testUsage("review-a"))
	if nonconverged, err := DecideDesign(testIdentity, reportSubject, []DesignReview{reportRevise}, 1, 1); err != nil || nonconverged.Outcome != OutcomeNonconverged {
		t.Fatalf("report last-round decision: %+v %v", nonconverged, err)
	}
	if _, err := DecideDesign(testIdentity, reportSubject, []DesignReview{reportPass, reportPass}, 1, 3); err == nil {
		t.Error("two reviews of a report were accepted")
	}

	// Tampering with the outcome or the review set is caught.
	tampered := revise
	tampered.Outcome = OutcomeApproved
	if err := tampered.Validate(testIdentity, subject, []DesignReview{passA, reviseB}, 3); err == nil || !strings.Contains(err.Error(), "outcome") {
		t.Errorf("tampered outcome: %v", err)
	}
	swapped := approved
	swapped.ReviewSHA256s = []string{passB.ReviewSHA256, passA.ReviewSHA256}
	if err := swapped.Validate(testIdentity, subject, []DesignReview{passA, passB}, 3); err == nil || !strings.Contains(err.Error(), "review set") {
		t.Errorf("swapped review set: %v", err)
	}
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "design-decision.json"), approved); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDesignDecision(filepath.Join(dir, "design-decision.json"))
	if err != nil || read.Validate(testIdentity, subject, []DesignReview{passA, passB}, 3) != nil {
		t.Errorf("round trip: %v", err)
	}
}
