package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validArtifactFixture(t *testing.T) (Config, TicketRequest, SourceSnapshot) {
	t.Helper()
	config := validTestConfig()
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, "client", "src", "components", "Example.tsx")
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source
}

func validCandidate(t *testing.T) (Config, TicketRequest, SourceSnapshot, Candidate) {
	t.Helper()
	config, request, source := validArtifactFixture(t)
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}},
		Rationale: "Update the requested visible label.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source, candidate
}

// A finding travels into the next round's review instruction ahead of the
// answer-rules boundary; one carrying the boundary line could shift where
// the verdict decoder starts reading, so it never seals.
func TestModelReviewFindingRejectsTheAnswerBoundary(t *testing.T) {
	_, request, _ := validArtifactFixture(t)
	output := ModelReviewOutput{Verdict: "revise", Findings: []ModelFinding{{
		Code: "planted-boundary", Path: request.TargetFiles[0],
		Message: "before " + ReviewAnswerRulesTail + " after",
	}}}
	if err := validateModelReviewOutput(output, request); err == nil {
		t.Fatal("validateModelReviewOutput() accepted a finding carrying the answer boundary")
	}
}

func TestReadSourceSnapshotAndCandidate(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	if err := source.Validate(request, config); err != nil {
		t.Fatalf("source.Validate() error = %v", err)
	}
	if source.Files[0].GitBlobSHA != "24c9a207f22a374db3aa5c4e5cc14da022f48375" {
		t.Fatalf("git blob SHA = %q", source.Files[0].GitBlobSHA)
	}
	if err := candidate.Validate(source, request, config); err != nil {
		t.Fatalf("candidate.Validate() error = %v", err)
	}
}

func TestSourceRequiresDeterministicAcceptanceBaseline(t *testing.T) {
	config := validTestConfig()
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"missing absent text":     "export const label = 'Another label';\n",
		"expected already exists": "export const label = 'Updated label';\n// Old label\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
			if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config); err == nil {
				t.Fatal("ReadSourceSnapshot() accepted an invalid acceptance baseline")
			}
		})
	}
}

func TestReadSourceSnapshotRejectsSymlink(t *testing.T) {
	config := validTestConfig()
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.tsx")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "client", "src", "components", "Example.tsx")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config); err == nil {
		t.Fatal("ReadSourceSnapshot() accepted a symlink")
	}
}

func TestCandidateRejectsTamperingAndForbiddenText(t *testing.T) {
	config, request, source, candidate := validCandidate(t)

	tampered := candidate
	tampered.Files = append([]CandidateFile(nil), candidate.Files...)
	tampered.Files[0].Content += "// tampered\n"
	if err := tampered.Validate(source, request, config); err == nil {
		t.Fatal("Candidate.Validate() accepted digest tampering")
	}
	tampered = candidate
	tampered.Invocation.RequestID = "request-substituted"
	if err := tampered.Validate(source, request, config); err == nil {
		t.Fatal("Candidate.Validate() accepted invocation tampering")
	}

	_, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "const name = 'FoRbIdDeN-PrOjEcT-NaMe';\n"}},
		Rationale: "Introduce a forbidden mechanism name.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err == nil {
		t.Fatal("NewCandidate() accepted forbidden text")
	}
}

func TestCandidateRequiresExactFileSetAndAChange(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	tests := []ModelCandidateOutput{
		{Files: nil, Rationale: "No files."},
		{Files: []ModelCandidateFile{{Path: request.TargetFiles[0], Content: source.Files[0].Content}}, Rationale: "No change."},
		{Files: []ModelCandidateFile{{Path: "client/src/unexpected.tsx", Content: "change"}}, Rationale: "Wrong file."},
	}
	for _, output := range tests {
		if _, err := NewCandidate(1, output, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime); err == nil {
			t.Fatalf("NewCandidate() accepted output %+v", output)
		}
	}
}

func TestCandidateRequiresDeterministicAcceptanceTransition(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	for name, content := range map[string]string{
		"old text retained": "export const label = 'Updated label';\n// Old label\n",
		"new text missing":  "export const label = 'Another label';\n",
	} {
		t.Run(name, func(t *testing.T) {
			output := ModelCandidateOutput{
				Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: content}},
				Rationale: "Attempt a visible label update.",
			}
			if _, err := NewCandidate(1, output, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime); err == nil {
				t.Fatal("NewCandidate() accepted an invalid acceptance transition")
			}
		})
	}
}

func TestCandidateEnforcesDeterministicChangeBudget(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.MaxChangedLines = 1
	config.Consumers[0].Mode.MaxChangedBytes = 8
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("Old label\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "Updated label with excess bytes\n"}},
		Rationale: "Replace too much under the configured budget.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime); err == nil {
		t.Fatal("NewCandidate() accepted a change over the deterministic budget")
	}
}

// Two one-line edits at opposite ends of a large file are a four-line
// change, not a whole-file change. The prefix/suffix reading measurably
// killed a valid candidate at 5,283 counted lines for exactly this shape:
// scattered template fixes across two locale dictionaries (2026-08-17).
func TestChangeBudgetCountsScatteredEditsNotTheSpanBetweenThem(t *testing.T) {
	lines := make([]string, 1000)
	for index := range lines {
		lines[index] = "line " + strings.Repeat("x", index%7)
	}
	before := strings.Join(lines, "\n") + "\n"
	lines[2] = "edited near the top"
	lines[997] = "edited near the bottom"
	after := strings.Join(lines, "\n") + "\n"
	changedLines, changedBytes, fallback := conservativeChangeBudget(before, after)
	if changedLines != 4 || fallback {
		t.Fatalf("changedLines = %d (fallback %v), want 4 from the subsequence search", changedLines, fallback)
	}
	if changedBytes <= 0 || changedBytes > 100 {
		t.Fatalf("changedBytes = %d, want the bytes of the four edited lines", changedBytes)
	}
	if l, b, _ := conservativeChangeBudget(before, before); l != 0 || b != 0 {
		t.Fatalf("identical contents budget = %d lines, %d bytes", l, b)
	}
	if l, _, _ := conservativeChangeBudget("", after); l != len(lines) {
		t.Fatalf("creation budget = %d lines, want every line", l)
	}
}

// A moved line is charged in both dimensions of the same reading. Optimizing
// lines and bytes separately let a crafted move of one huge line report two
// changed lines alongside the bytes of an unrelated, cheaper reading -
// passing a byte gate no consistent diff of the change could pass (found in
// adversarial review, 2026-08-17).
func TestChangeBudgetChargesAMovedLineInBothDimensions(t *testing.T) {
	huge := strings.Repeat("h", 100_000) + "\n"
	short := make([]string, 2000)
	for index := range short {
		short[index] = "short line " + strings.Repeat("y", index%9) + "\n"
	}
	body := strings.Join(short, "")
	before := huge + body
	after := body + huge
	changedLines, changedBytes, fallback := conservativeChangeBudget(before, after)
	if fallback {
		t.Fatal("the move fixture must stay inside the subsequence search")
	}
	if changedLines != 2 {
		t.Fatalf("changedLines = %d, want the moved line charged once per side", changedLines)
	}
	if changedBytes != 2*len(huge) {
		t.Fatalf("changedBytes = %d, want %d - the moved line's bytes on both sides", changedBytes, 2*len(huge))
	}
}

// Beyond the cell bound the search falls back to counting the whole trimmed
// middle - the pre-subsequence reading - and says so, so a refusal for a
// small edit in an enormous file reads as the fallback it is.
func TestChangeBudgetFallsBackConservativelyPastTheCellBound(t *testing.T) {
	lines := make([]string, 9001)
	for index := range lines {
		lines[index] = "row " + strings.Repeat("z", index%11) + "\n"
	}
	before := strings.Join(lines, "")
	lines[1] = "edited near the top\n"
	lines[8999] = "edited near the bottom\n"
	after := strings.Join(lines, "")
	changedLines, _, fallback := conservativeChangeBudget(before, after)
	if !fallback {
		t.Fatalf("a %d-line middle must exceed the cell bound", 8999)
	}
	if changedLines != 2*8999 {
		t.Fatalf("changedLines = %d, want the whole trimmed middle of both sides", changedLines)
	}
}

func TestReviewsAndStageDecision(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for _, endpoint := range config.Models.Reviewers {
		review, err := NewReview(1, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != "converged" {
		t.Fatalf("outcome = %q", decision.Outcome)
	}
	if err := decision.Validate(candidate, reviews, source, request, config); err != nil {
		t.Fatal(err)
	}
	tamperedDecision := decision
	tamperedDecision.Outcome = "revise"
	if err := tamperedDecision.Validate(candidate, reviews, source, request, config); err == nil {
		t.Fatal("StageDecision.Validate() accepted outcome tampering")
	}
	tampered := reviews[0]
	tampered.Invocation.RequestedModel = "another-model"
	if _, err := DecideStage(candidate, []Review{tampered, reviews[1]}, source, request, config, nil); err == nil {
		t.Fatal("DecideStage() accepted review invocation tampering")
	}
}

func TestStageDecisionRejectsDuplicateInvocationRequestID(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for _, endpoint := range config.Models.Reviewers {
		invocation := validTestInvocation(endpoint)
		invocation.RequestID = candidate.Invocation.RequestID
		review, err := NewReview(1, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}, candidate, source, request, config, invocation, testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	if _, err := DecideStage(candidate, reviews, source, request, config, nil); err == nil {
		t.Fatal("DecideStage() accepted duplicate model request ids")
	}
}

// A reviewer that passes a change and still writes a note has passed it, and
// one that asks for a revision without listing a finding has still asked.
// Both were thrown away and asked for again, and asking again is what rounds
// are spent on. A verdict that is neither word is still no answer.
func TestAReviewIsTakenAsGivenUnlessTheVerdictIsUnreadable(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	endpoint := config.Models.Reviewers[0]
	for _, output := range []ModelReviewOutput{
		{Verdict: "pass", Findings: []ModelFinding{{Code: "bug", Path: request.TargetFiles[0], Message: "Bug."}}},
		{Verdict: "revise", Findings: nil},
	} {
		if _, err := NewReview(1, endpoint, output, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime); err != nil {
			t.Fatalf("NewReview() refused %+v: %v", output, err)
		}
	}
	unreadable := ModelReviewOutput{Verdict: "maybe", Findings: nil}
	if _, err := NewReview(1, endpoint, unreadable, candidate, source, request, config, validTestInvocation(endpoint), testInvocationTime); err == nil {
		t.Fatal("NewReview() accepted a verdict that is neither pass nor revise")
	}
}

func TestReviewRejectsTimestampBeforeCandidate(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	endpoint := config.Models.Reviewers[0]
	if _, err := NewReview(
		1, endpoint, ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}},
		candidate, source, request, config, validTestInvocation(endpoint),
		candidate.GeneratedAt.Add(-allowedArtifactClockSkew-time.Second),
	); err == nil {
		t.Fatal("NewReview() accepted a review timestamp before candidate generation")
	}
}

// A round at the destination's declared budget is a round to do again, and
// nothing more.
//
// It used to become nonconverged here, and that is how a delivery ended on a
// count: the decide verb rewrote the last allowed round's objection into an
// ending, and the requester was told the reviews had not converged. Rounds
// are not counted out any more, so the objection stays what it is.
func TestTheDeclaredBudgetNoLongerRewritesAnObjectionIntoAnEnding(t *testing.T) {
	config, request, source, first := validCandidate(t)
	last, err := NewCandidate(config.MaxStages, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: first.Files[0].Content}},
		Rationale: "Last allowed stage.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	reviews := make([]Review, 0, len(config.Models.Reviewers))
	for index, endpoint := range config.Models.Reviewers {
		output := ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}
		if index == 0 {
			output = ModelReviewOutput{Verdict: "revise", Findings: []ModelFinding{{
				Code: "still-broken", Path: request.TargetFiles[0], Message: "The acceptance criterion is still not met.",
			}}}
		}
		review, err := NewReview(config.MaxStages, endpoint, output, last, source, request, config, validTestInvocation(endpoint), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := DecideStage(last, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != "revise" {
		t.Fatalf("outcome = %q, want revise", decision.Outcome)
	}
	// And a round past the declared budget seals at all: an existing
	// destination declaring three rounds does not refuse the fourth.
	beyond := config.MaxStages + 1
	if _, err := NewCandidate(beyond, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: first.Files[0].Content}},
		Rationale: "Past the declared budget.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime); err != nil {
		t.Fatalf("round %d was refused by a configuration declaring %d: %v", beyond, config.MaxStages, err)
	}
}

// A model answer is read for what the engine needs: a key the shape does not
// carry, and a value written after the answer, are read past rather than
// costing the round the answer was given in (internal/modeljson). What the
// answer says is still held to the contract by the validators.
func TestAModelResponseIsReadPastItsShape(t *testing.T) {
	candidate, err := DecodeModelCandidateOutput([]byte(`{"files":[],"rationale":"x","extra":true}`))
	if err != nil || candidate.Rationale != "x" {
		t.Fatalf("an unknown key must be read past: %v, %+v", err, candidate)
	}
	review, err := DecodeModelReviewOutput([]byte(`{"verdict":"pass","findings":[]} {}`))
	if err != nil || review.Verdict != "pass" {
		t.Fatalf("a value written after the answer must be read past: %v, %+v", err, review)
	}
	// A needed field of the wrong type has nothing in it to read.
	if _, err := DecodeModelCandidateOutput([]byte(`{"files":{"path":"x"},"rationale":"x"}`)); err == nil {
		t.Fatal("a files field that is not a list was accepted")
	}
}

// TestSnapshotAndCandidateWithoutAWordingPromise: a ticket that promises
// behavior rather than wording still snapshots and validates — the wording
// checks simply do not apply, while the change/limit checks all do.
// A created file travels the whole sealed chain: observation with an empty
// before-side, a source entry flagged Created, a candidate that changes it,
// and an apply that writes the new path exactly once. Adding a numbered
// migration file is ordinary development, and the first live migration
// ticket measurably died before this existed.
func TestCreatedFileFlowsFromObservationToApply(t *testing.T) {
	config := validTestConfig()
	created := "client/src/routes/retry.ts"
	content := "export const retryRoute = true;\n"
	request := TicketRequest{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ToolSHA: strings.Repeat("2", 40),
		IssueKey: "TICKET-9", RunID: "run_20260806_general",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary:     "Add a retry route for notifications",
		TargetFiles: []string{created},
		Request:     "Add a retry route so a failed notification is delivered again.",
	}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = configSHA
	if err := request.Validate(config); err != nil {
		t.Fatal(err)
	}

	observed := []ObservedChange{{Path: created, After: []byte(content), Created: true}}
	source, err := SourceFromObservedChanges(strings.Repeat("b", 40), observed, request, config)
	if err != nil {
		t.Fatalf("SourceFromObservedChanges() error = %v", err)
	}
	if !source.Files[0].Created || source.Files[0].Content != "" {
		t.Fatalf("source = %+v, want a created entry with empty content", source.Files[0])
	}

	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: created, Content: content}},
		Rationale: "Add the retry route as a new file.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatalf("NewCandidate() error = %v", err)
	}

	root := t.TempDir()
	if err := ApplyCandidate(root, candidate, source, request, config); err != nil {
		t.Fatalf("ApplyCandidate() error = %v", err)
	}
	if err := VerifyApplied(root, candidate, source, request, config); err != nil {
		t.Fatalf("VerifyApplied() error = %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(created)))
	if err != nil || string(written) != content {
		t.Fatalf("written = %q, %v", written, err)
	}
	if err := ApplyCandidate(root, candidate, source, request, config); err == nil {
		t.Fatal("a second apply over the now-existing file must be rejected")
	}
}

func TestSnapshotAndCandidateWithoutAWordingPromise(t *testing.T) {
	config := validTestConfig()
	request := TicketRequest{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ToolSHA: strings.Repeat("2", 40),
		IssueKey: "TICKET-9", RunID: "run_20260806_general",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary:     "Add a retry route for notifications",
		TargetFiles: []string{"client/src/components/Example.tsx"},
		Request:     "Add a retry route so a failed notification is delivered again.",
	}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = configSHA
	if err := request.Validate(config); err != nil {
		t.Fatalf("a ticket without a wording promise must validate: %v", err)
	}
	if request.HasWordingPromise() {
		t.Fatal("fixture must carry no wording promise")
	}

	root := t.TempDir()
	filename := filepath.Join(root, "client", "src", "components", "Example.tsx")
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const retries = 0;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatalf("ReadSourceSnapshot() error = %v", err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const retries = 3;\n"}},
		Rationale: "Retry failed notifications three times.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatalf("NewCandidate() error = %v", err)
	}
	if err := candidate.Validate(source, request, config); err != nil {
		t.Fatalf("candidate.Validate() error = %v", err)
	}

	unchanged, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const retries = 0;\n"}},
		Rationale: "No change.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err == nil {
		t.Fatalf("an unchanged candidate must still be refused, got %+v", unchanged)
	}
}

// The Created flag must not disturb the sealed identity of anything that
// existed before it: a non-created source file marshals byte-identically to
// the pre-flag encoding, so every digest sealed before the flag stays valid.
func TestSourceFileEncodingWithoutCreatedIsUnchanged(t *testing.T) {
	encoded, err := json.Marshal(SourceFile{Path: "a.ts", GitBlobSHA: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Content: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"path":"a.ts","git_blob_sha":"` + strings.Repeat("a", 40) + `","sha256":"` + strings.Repeat("b", 64) + `","content":"hi"}`
	if string(encoded) != expected {
		t.Fatalf("encoding drifted: %s", encoded)
	}
}

// A committed symlink directory inside the writable scope must not let a
// created file land outside the apply root - the escape was measurably
// reproduced before the created-path walk existed.
func TestApplyCandidateRejectsCreatedFileThroughSymlinkedDirectory(t *testing.T) {
	config := validTestConfig()
	created := "client/src/migrations/0001_init.sql"
	request := TicketRequest{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ToolSHA: strings.Repeat("2", 40),
		IssueKey: "TICKET-9", RunID: "run_20260806_general",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary:     "Add a migration",
		TargetFiles: []string{created},
		Request:     "Add the initial migration as a new file.",
	}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = configSHA
	observed := []ObservedChange{{Path: created, After: []byte("-- init\n"), Created: true}}
	source, err := SourceFromObservedChanges(strings.Repeat("b", 40), observed, request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files:     []ModelCandidateFile{{Path: created, Content: "-- init\n"}},
		Rationale: "Add the initial migration.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "client", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "client", "src", "migrations")); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCandidate(root, candidate, source, request, config); err == nil {
		t.Fatal("a created file routed through a symlinked directory was applied")
	}
	if _, err := os.Stat(filepath.Join(outside, "0001_init.sql")); !os.IsNotExist(err) {
		t.Fatal("the escape write reached outside the root")
	}
}

// A target the change is to create has no before-side. The snapshot used to
// refuse the run for it, so a request to create a file died one step after
// the file choice, with the reason only in the log.
func TestASnapshotRecordsATargetThatDoesNotExistYet(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "README.md"), []byte("# hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file this run creates: absent from the base, inside the tree.
	created, err := newFileTarget(root, "docs/NEW.md", WorkflowAllowance{})
	if err != nil || !created {
		t.Fatalf("an absent target: created=%v err=%v", created, err)
	}
	// One that exists is not a file to create.
	if created, err := newFileTarget(root, "docs/README.md", WorkflowAllowance{}); err == nil || created {
		t.Fatalf("an existing target: created=%v err=%v", created, err)
	}
	// A component in the way, or a symbolic link, is still a refusal.
	if err := os.WriteFile(filepath.Join(root, "docs", "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if created, err := newFileTarget(root, "docs/blocker/NEW.md", WorkflowAllowance{}); err == nil || created {
		t.Fatalf("a file in the path: created=%v err=%v", created, err)
	}
	if err := os.Symlink(filepath.Join(root, "docs"), filepath.Join(root, "link")); err == nil {
		if created, err := newFileTarget(root, "link/NEW.md", WorkflowAllowance{}); err == nil || created {
			t.Fatalf("a symbolic link in the path: created=%v err=%v", created, err)
		}
	}
	// The reader itself records it: a request naming a file the change is to
	// create returns a snapshot with an empty before-side, where it used to
	// refuse the run one step after the file was chosen.
	config := validTestConfig()
	request := TicketRequest{
		SchemaVersion: 1, DeliveryID: "delivery_0123456789abcdef0123456789abcdef",
		InputSHA256: strings.Repeat("1", 64), ToolSHA: strings.Repeat("2", 40),
		IssueKey: "TICKET-9", RunID: "run_20260806_general",
		Repository: config.Consumers[0].Repository, Mode: config.Consumers[0].Mode.ID,
		Summary:     "Add a retry route for notifications",
		TargetFiles: []string{"client/src/routes/retry.ts"},
		Request:     "client/src/routes/retry.ts を新しく作ってください。",
	}
	configSHA, err := config.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = configSHA
	snapshot, err := ReadSourceSnapshot(root, strings.Repeat("b", 40), request, config)
	if err != nil {
		t.Fatalf("ReadSourceSnapshot for a file to create: %v", err)
	}
	if len(snapshot.Files) != 1 || !snapshot.Files[0].Created || snapshot.Files[0].Content != "" {
		t.Fatalf("snapshot = %+v", snapshot.Files)
	}

	// And a hidden or escaping path is refused whatever the tree looks like.
	for _, refused := range []string{"docs/.hidden.md", "../escape.md", "/etc/passwd"} {
		if created, err := newFileTarget(root, refused, WorkflowAllowance{}); err == nil || created {
			t.Errorf("%q: created=%v err=%v", refused, created, err)
		}
	}
}
