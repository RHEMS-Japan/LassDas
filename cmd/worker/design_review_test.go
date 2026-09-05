package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// designFixture is the agent fixture plus what the investigate stage would
// have left in the run directory: real measurements of the fixture
// repository, a sealed investigation report over them and a sealed design.
type designFixture struct {
	agentFixture
	identity          investigate.Identity
	measurementsPath  string
	investigationPath string
	designPath        string
	investigation     investigate.Investigation
	design            investigate.Design
}

const passVerdict = `echo '{"verdict":"pass","findings":[]}'`

func newDesignFixture(t *testing.T, reviewerBody string, tune func(binaries string, config *worker.Config)) designFixture {
	t.Helper()
	fixture := designFixture{agentFixture: newTunedAgentFixture(t, "true", reviewerBody, tune)}
	configSHA, err := fixture.config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	fixture.identity = investigate.Identity{
		DeliveryID: "delivery_" + strings.Repeat("d", 32), InputSHA256: strings.Repeat("1", 64),
		ConfigSHA256: configSHA, ToolSHA: cliToolSHA, BaseSHA: fixture.baseSHA,
	}
	catalog, err := probe.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.measurementsPath = fixture.path("measurements.jsonl")
	recorder, err := probe.OpenRecorder(fixture.measurementsPath)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: fixture.repoRoot}
	for _, request := range []probe.Request{
		{Probe: "repo.list"},
		{Probe: "repo.read", Args: map[string]string{"path": "client/src/label.ts"}},
	} {
		if _, err := session.Run(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	fixture.investigation, err = investigate.NewInvestigation(fixture.identity, 1, investigate.ModelInvestigationOutput{
		Questions: []string{"Where does the visible label live?"},
		Findings: []investigate.Finding{
			{Claim: "The label is hard-coded in client/src/label.ts", Evidence: []string{"m-0002"}, Confidence: investigate.ConfidenceMeasured},
		},
		Unknowns: []string{"Whether any caller compares against the old text"},
		Next:     "Replace the label text in place.",
	}, fixture.measurementsPath, 2, investigate.Budget{ProbesUsed: 2, ElapsedSeconds: 3})
	if err != nil {
		t.Fatal(err)
	}
	fixture.design, err = investigate.NewDesign(fixture.identity, 1, investigate.ModelDesignOutput{
		Cause: "The visible label is a hard-coded string", CauseEvidence: []string{"m-0002"},
		Approach: "Replace the string in place", Alternatives: []string{"Introduce a translation table"},
		Files:        []investigate.FileChange{{Path: "client/src/label.ts", Changes: []string{"replace Old label with Updated label"}}},
		Verification: investigate.Verification{Form: investigate.VerificationWording, Path: "/settings", ExpectedText: "Updated label", AbsentText: "Old label"},
		BlastRadius:  []string{"the settings page heading"}, NotDoing: []string{"renaming the export"},
	}, fixture.investigation, investigate.Bounds{AllowedFilePrefixes: []string{"client/src/"}, MaxFiles: 3, Catalog: catalog, RepoRoot: fixture.repoRoot})
	if err != nil {
		t.Fatal(err)
	}
	fixture.investigationPath = fixture.path("investigation-1.json")
	fixture.designPath = fixture.path("design-1.json")
	if err := investigate.Write(fixture.investigationPath, fixture.investigation); err != nil {
		t.Fatal(err)
	}
	if err := investigate.Write(fixture.designPath, fixture.design); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// review runs agent-design-review for one reviewer; label names the output
// files so one fixture can run several reviews.
func (f designFixture) review(t *testing.T, label, reviewerID string, withDesign bool, extra ...string) error {
	t.Helper()
	args := []string{
		"agent-design-review", "--config", f.configPath, "--tool-sha", cliToolSHA,
		"--investigation", f.investigationPath, "--measurements", f.measurementsPath,
		"--repo-root", f.repoRoot, "--base-sha", f.baseSHA, "--reviewer", reviewerID,
		"--run-out", f.path(label + "-run.json"), "--out", f.path(label + ".json"),
	}
	if withDesign {
		args = append(args, "--design", f.designPath)
	}
	return run(context.Background(), append(args, extra...))
}

func (f designFixture) readReview(t *testing.T, label string) investigate.DesignReview {
	t.Helper()
	var review investigate.DesignReview
	readAgentArtifact(t, f.path(label+".json"), worker.MaxReviewJSONBytes, &review)
	return review
}

func (f designFixture) decide(t *testing.T, label string, round int, withDesign bool, reviewLabels ...string) error {
	t.Helper()
	args := []string{
		"decide-design", "--config", f.configPath, "--tool-sha", cliToolSHA,
		"--investigation", f.investigationPath, "--measurements", f.measurementsPath,
		"--round", strconv.Itoa(round), "--out", f.path(label + ".json"),
	}
	if withDesign {
		args = append(args, "--design", f.designPath)
	}
	for _, reviewLabel := range reviewLabels {
		args = append(args, "--review", f.path(reviewLabel+".json"))
	}
	return run(context.Background(), args)
}

func (f designFixture) readDecision(t *testing.T, label string) investigate.DesignDecision {
	t.Helper()
	var decision investigate.DesignDecision
	readAgentArtifact(t, f.path(label+".json"), worker.MaxDecisionJSONBytes, &decision)
	return decision
}

func TestAgentDesignReviewSealsTheVerdictTheReviewerPrinted(t *testing.T) {
	fixture := newDesignFixture(t, `echo "I read the design and the template."; `+passVerdict, nil)
	if err := fixture.review(t, "design-review-a", "review-a", true); err != nil {
		t.Fatal(err)
	}
	review := fixture.readReview(t, "design-review-a")
	if review.Verdict != investigate.VerdictPass || review.Subject != investigate.SubjectDesign || review.SubjectSHA256 != fixture.design.DesignSHA256 ||
		review.ReviewerID != "review-a" || review.Lens != worker.DesignLensEvidence || review.Round != 1 {
		t.Fatalf("the verdict was not sealed against the design: %+v", review)
	}
	if err := review.Validate(fixture.identity, investigate.DesignSubject(fixture.design)); err != nil {
		t.Fatalf("the sealed review was rejected: %v", err)
	}
	var record worker.AgentRun
	readAgentArtifact(t, fixture.path("design-review-a-run.json"), worker.MaxArtifactJSONBytes, &record)
	if record.AgentID != "review-agent" || record.Stage != 1 || record.DeliveryID != fixture.identity.DeliveryID || review.Invocation.RequestID != record.RunSHA256 {
		t.Fatalf("the run record does not carry the review: %+v", record)
	}
}

func TestAgentDesignReviewSealsAReviseWithItsSection(t *testing.T) {
	fixture := newDesignFixture(t,
		`echo '{"verdict":"revise","findings":[{"code":"unjudgeable-promise","section":"verification","message":"The screen check names a page the changed file does not render."}]}'`, nil)
	if err := fixture.review(t, "design-review-b", "review-b", true); err != nil {
		t.Fatal(err)
	}
	review := fixture.readReview(t, "design-review-b")
	if review.Verdict != investigate.VerdictRevise || len(review.Findings) != 1 || review.Findings[0].Section != investigate.SectionVerification ||
		review.Findings[0].Code != "unjudgeable-promise" || review.Lens != worker.DesignLensApproach {
		t.Fatalf("the objection was not sealed: %+v", review)
	}
	if err := review.Validate(fixture.identity, investigate.DesignSubject(fixture.design)); err != nil {
		t.Fatalf("the sealed review was rejected: %v", err)
	}
}

// A transcript outside the contract - the candidate review's fields, an
// extra field, a pass that still objects, a section the subject does not
// have, no verdict at all - seals nothing.
func TestAgentDesignReviewRefusesAVerdictOutsideTheContract(t *testing.T) {
	bodies := map[string]string{
		"a finding with a path":    `echo '{"verdict":"revise","findings":[{"code":"stale-caller","section":"files","message":"m","path":"client/src/label.ts"}]}'`,
		"an extra field":           `echo '{"verdict":"pass","findings":[],"confidence":"high"}'`,
		"a pass that objects":      `echo '{"verdict":"pass","findings":[{"code":"x-y","section":"cause","message":"m"}]}'`,
		"a section of a report":    `echo '{"verdict":"revise","findings":[{"code":"x-y","section":"unknowns","message":"m"}]}'`,
		"a missing section":        `echo '{"verdict":"revise","findings":[{"code":"x-y","message":"m"}]}'`,
		"prose without a verdict":  `echo "The design reads well to me."`,
		"an unknown verdict value": `echo '{"verdict":"approve","findings":[]}'`,
	}
	for name, body := range bodies {
		fixture := newDesignFixture(t, body, nil)
		if err := fixture.review(t, "design-review-a", "review-a", true); err == nil {
			t.Errorf("%s was sealed as a review", name)
		}
		if _, err := os.Stat(fixture.path("design-review-a.json")); err == nil {
			t.Errorf("%s left a review artifact behind", name)
		}
	}
}

func TestAgentDesignReviewRejectsAReviewerThatEditsTheTree(t *testing.T) {
	fixture := newDesignFixture(t, `printf "export const label = 'Reviewer wrote this';\n" > client/src/label.ts; `+passVerdict, nil)
	if err := fixture.review(t, "design-review-a", "review-a", true); err == nil || !strings.Contains(err.Error(), "changed the tree") {
		t.Fatalf("a reviewer that edited the baseline: %v", err)
	}
	if _, err := os.Stat(fixture.path("design-review-a.json")); err == nil {
		t.Fatal("a review was written for a reviewer that edited the tree")
	}
}

// The reviewer is pointed at the measurements file so it can read the full
// outputs. One that rewrites a sealed line instead breaks the chain the
// report is bound to, and the review is refused.
func TestAgentDesignReviewRejectsAReviewerThatRewritesTheMeasurements(t *testing.T) {
	fixture := newDesignFixture(t,
		`head -n 1 "$MEASUREMENTS" > "$MEASUREMENTS.tmp"; `+
			`printf '%s\n' '{"id":"m-0002","probe":"repo.read","output":"rewritten","line_sha256":"0","chain_sha256":"0"}' >> "$MEASUREMENTS.tmp"; `+
			`cat "$MEASUREMENTS.tmp" > "$MEASUREMENTS"; rm "$MEASUREMENTS.tmp"; `+passVerdict,
		func(binaries string, config *worker.Config) {
			config.Agents.Reviewer.Env = map[string]string{"MEASUREMENTS": filepath.Join(filepath.Dir(binaries), "measurements.jsonl")}
		})
	if err := fixture.review(t, "design-review-a", "review-a", true); err == nil || !strings.Contains(err.Error(), "measurements changed") {
		t.Fatalf("a reviewer that rewrote the measurements: %v", err)
	}
	if _, err := os.Stat(fixture.path("design-review-a.json")); err == nil {
		t.Fatal("a review was written over rewritten measurements")
	}
}

// The first reviewer judges the evidence and the second the approach unless
// told otherwise; a report is judged under the evidence lens only.
func TestAgentDesignReviewResolvesTheLens(t *testing.T) {
	fixture := newDesignFixture(t, passVerdict, func(_ string, config *worker.Config) {
		config.Models.Reviewers[0].DesignLens = "Does every measured claim cite an output that says what the claim says?"
	})
	if err := fixture.review(t, "design-b", "review-b", true); err != nil {
		t.Fatal(err)
	}
	if lens := fixture.readReview(t, "design-b").Lens; lens != worker.DesignLensApproach {
		t.Fatalf("the second reviewer's default lens = %q", lens)
	}
	if err := fixture.review(t, "design-b-as-a", "review-b", true, "--lens", "A"); err != nil {
		t.Fatal(err)
	}
	if lens := fixture.readReview(t, "design-b-as-a").Lens; lens != worker.DesignLensEvidence {
		t.Fatalf("the selected lens = %q", lens)
	}
	if err := fixture.review(t, "design-a", "review-a", true); err != nil {
		t.Fatal(err)
	}
	if lens := fixture.readReview(t, "design-a").Lens; lens != fixture.config.Models.Reviewers[0].DesignLens {
		t.Fatalf("the configured lens was not used: %q", lens)
	}
	if err := fixture.review(t, "report-b", "review-b", false); err == nil {
		t.Fatal("the approach lens was accepted for an investigation report")
	}
	if err := fixture.review(t, "report-a", "review-a", false); err != nil {
		t.Fatal(err)
	}
	report := fixture.readReview(t, "report-a")
	if report.Subject != investigate.SubjectInvestigation || report.SubjectSHA256 != fixture.investigation.InvestigationSHA256 {
		t.Fatalf("the report review is not bound to the report: %+v", report)
	}
	if err := report.Validate(fixture.identity, investigate.InvestigationSubject(fixture.investigation)); err != nil {
		t.Fatalf("the sealed report review was rejected: %v", err)
	}
}

func TestAgentDesignReviewFeedsPreviousFindingsToTheJudge(t *testing.T) {
	fixture := newDesignFixture(t,
		`case "$*" in *from-earlier-round*) echo "SAW-PREVIOUS-FINDING";; esac; `+passVerdict, nil)
	previous := fixture.path("design-review-b-round-0.json")
	writeTestJSON(t, previous, map[string]any{"findings": []map[string]any{
		{"code": "from-earlier-round", "section": "files", "message": "A caller of the label is not among the files."},
	}})
	if err := fixture.review(t, "design-review-a", "review-a", true, "--previous-findings", previous); err != nil {
		t.Fatal(err)
	}
	var record worker.AgentRun
	readAgentArtifact(t, fixture.path("design-review-a-run.json"), worker.MaxArtifactJSONBytes, &record)
	if !strings.Contains(record.Transcript, "SAW-PREVIOUS-FINDING") {
		t.Fatalf("the judge was not told the earlier objection: %q", record.Transcript)
	}
}

// The stand-in objects under the approach lens only, so one fixture yields
// a pass from the evidence reviewer and a revise from the approach reviewer.
const objectUnderApproachLens = `case "$*" in *より小さい直し方*) echo '{"verdict":"revise","findings":[{"code":"missing-caller","section":"files","message":"A caller of the label is not among the files."}]}';; *) echo '{"verdict":"pass","findings":[]}';; esac`

func TestDecideDesignSealsTheRoundOutcome(t *testing.T) {
	approved := newDesignFixture(t, passVerdict, nil)
	for _, reviewer := range []string{"review-a", "review-b"} {
		if err := approved.review(t, "design-"+reviewer, reviewer, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := approved.decide(t, "design-decision", 1, true, "design-review-a", "design-review-b"); err != nil {
		t.Fatal(err)
	}
	decision := approved.readDecision(t, "design-decision")
	if decision.Outcome != investigate.OutcomeApproved || decision.SubjectSHA256 != approved.design.DesignSHA256 || len(decision.ReviewSHA256s) != 2 {
		t.Fatalf("approved decision: %+v", decision)
	}
	reviews := []investigate.DesignReview{approved.readReview(t, "design-review-a"), approved.readReview(t, "design-review-b")}
	if err := decision.Validate(approved.identity, investigate.DesignSubject(approved.design), reviews, approved.config.DesignRounds()); err != nil {
		t.Fatalf("the sealed decision was rejected: %v", err)
	}
	// One review is not a decision; a round the subject is not in is refused; a
	// report review is not a review of the design.
	if err := approved.decide(t, "half-decision", 1, true, "design-review-a"); err == nil {
		t.Fatal("a decision was sealed from one review")
	}
	if err := approved.decide(t, "wrong-round", 2, true, "design-review-a", "design-review-b"); err == nil {
		t.Fatal("a decision was sealed for a round the subject is not in")
	}
	if err := approved.review(t, "report-review-a", "review-a", false); err != nil {
		t.Fatal(err)
	}
	if err := approved.decide(t, "mixed-decision", 1, true, "report-review-a", "design-review-b"); err == nil {
		t.Fatal("a review of the report was counted toward the design")
	}
	if err := approved.decide(t, "report-decision", 1, false, "report-review-a"); err != nil {
		t.Fatal(err)
	}
	if outcome := approved.readDecision(t, "report-decision").Outcome; outcome != investigate.OutcomeApproved {
		t.Fatalf("report decision outcome = %q", outcome)
	}

	// One veto sends the design back while rounds remain.
	revised := newDesignFixture(t, objectUnderApproachLens, nil)
	for _, reviewer := range []string{"review-a", "review-b"} {
		if err := revised.review(t, "design-"+reviewer, reviewer, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := revised.decide(t, "design-decision", 1, true, "design-review-a", "design-review-b"); err != nil {
		t.Fatal(err)
	}
	if outcome := revised.readDecision(t, "design-decision").Outcome; outcome != investigate.OutcomeRevise {
		t.Fatalf("revise decision outcome = %q", outcome)
	}

	// At the configured round limit the same veto ends the delivery honestly.
	last := newDesignFixture(t, objectUnderApproachLens, func(_ string, config *worker.Config) { config.DesignMaxRounds = 1 })
	for _, reviewer := range []string{"review-a", "review-b"} {
		if err := last.review(t, "design-"+reviewer, reviewer, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := last.decide(t, "design-decision", 1, true, "design-review-a", "design-review-b"); err != nil {
		t.Fatal(err)
	}
	if outcome := last.readDecision(t, "design-decision").Outcome; outcome != investigate.OutcomeNonconverged {
		t.Fatalf("last-round decision outcome = %q", outcome)
	}
}

// Sixty measurements of full excerpts outgrow the instruction; the oldest
// lose their excerpts first and the newest keeps its own.
func TestDesignReviewPromptWithdrawsExcerptsToFitTheBudget(t *testing.T) {
	measurements := make([]probe.Measurement, 0, 60)
	for index := 1; index <= 60; index++ {
		measurements = append(measurements, probe.Measurement{
			ID: fmt.Sprintf("m-%04d", index), Probe: "repo.read", Args: map[string]string{"path": "web/page.tmpl"},
			Output: strings.Repeat("あ", 2048), OutputBytes: 6144,
		})
	}
	prompt, err := designReviewPrompt(designReviewPromptInput{
		subject:          investigate.ReviewSubject{Kind: investigate.SubjectInvestigation, Round: 1, SHA256: strings.Repeat("e", 64)},
		lens:             worker.DesignLensEvidence,
		investigation:    investigate.Investigation{Round: 1, Questions: []string{"q"}, Next: "n"},
		measurements:     measurements,
		measurementsPath: "/run/measurements.jsonl",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > worker.MaxAgentPromptBytes {
		t.Fatalf("prompt bytes = %d", len(prompt))
	}
	withdrawn := strings.Count(prompt, `"excerpt_withdrawn":true`)
	if withdrawn == 0 || withdrawn >= 60 {
		t.Fatalf("withdrawn excerpts = %d", withdrawn)
	}
	if !strings.Contains(prompt, `"id":"m-0060","probe":"repo.read"`) || !strings.Contains(prompt, `"excerpts_withdrawn":`+strconv.Itoa(withdrawn)) {
		t.Fatal("the newest measurement lost its excerpt, or the count is not stated")
	}
	if !strings.Contains(prompt, "/run/measurements.jsonl") || !strings.HasSuffix(prompt, worker.ReviewAnswerRulesTail) {
		t.Fatal("the instruction does not point at the measurements file or does not end in the answer-rules boundary")
	}
}
