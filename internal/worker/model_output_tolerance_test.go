package worker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The strictness these tests remove was measured, not hypothetical: three
// live requests that genuinely lacked information — the requests the
// reception's single round of questions exists for — died at the reception
// inside two minutes each because the reader named a gap's field with a word
// outside IntakeGap, and a stronger model named it the same way. Every test
// here fails without the tolerant reading.

// TestAGapWithAnExtraFieldIsReadAsTheGapItIs is the measured failure itself.
func TestAGapWithAnExtraFieldIsReadAsTheGapItIs(t *testing.T) {
	config := validTestConfig()
	// Two destinations, so the one gap intake keeps — which repository — is a
	// real question rather than something the configuration answers by
	// itself.
	config.Consumers = append(config.Consumers, secondTestConsumer())
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	// The gap the reader actually wrote: the three fields the engine reads,
	// plus a reason of its own.
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"repository":"","verification_path":"","expected_text":"","absent_text":"",` +
			`"request":"用語を日本語の説明に置き換える",` +
			`"gaps":[{"field":"repository","question":"どのリポジトリですか","choices":[` +
			`{"id":"a","label":"one","effect":"one で変更する"},` +
			`{"id":"b","label":"two","effect":"two で変更する"}],"reason":"本文が指していない"}],` +
			`"rationale":"完了条件から読んだ"}`,
	)})

	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("a gap carrying one extra field must be read as the gap it is: %v", err)
	}
	if len(intake.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want the one the reader raised", intake.Gaps)
	}
	gap := intake.Gaps[0]
	if gap.Field != "repository" || gap.Question != "どのリポジトリですか" || len(gap.Choices) != 2 || gap.Choices[0].ID != "a" {
		t.Fatalf("gap = %+v", gap)
	}
}

// TestAnIntakeAnswerWrappedInProseIsRead: models write a sentence before the
// JSON even under a response schema.
func TestAnIntakeAnswerWrappedInProseIsRead(t *testing.T) {
	config := validTestConfig()
	raw, err := ReadRawTicket(validTicketEnvelope(t, realTicketDescription()), config, strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		"Here is the JSON:\n```json\n" +
			`{"repository":"","verification_path":"/settings","expected_text":"1分あたりのリクエスト数上限","absent_text":"RPM制限",` +
			`"request":"用語を日本語の説明に置き換える","gaps":[],"rationale":"完了条件から読んだ"}` +
			"\n```\nLet me know if anything is unclear.",
	)})

	intake, _, err := invoker.ReadContract(context.Background(), raw, config)
	if err != nil {
		t.Fatalf("an answer wrapped in prose must be read: %v", err)
	}
	if intake.AbsentText != "RPM制限" || intake.ExpectedText != "1分あたりのリクエスト数上限" || intake.VerificationPath != "/settings" {
		t.Fatalf("intake = %+v", intake)
	}
	if intake.Fallback {
		t.Fatal("an answer that was read is not a reading made without the model")
	}
}

// TestAReadinessAnswerWithExtraFieldsIsRead covers the assessor: two live
// tickets already died on a mislabeled enum-ish field, which is why those are
// coerced; a key the shape does not carry is the same class of loss.
func TestAReadinessAnswerWithExtraFieldsIsRead(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"decision":"ready","questions":[],"assumptions":[],"reject_code":"","confidence":0.82,"notes":"looks fine"}`,
	)})

	assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatalf("an assessment carrying extra keys must be read: %v", err)
	}
	if assessment.Decision != ReadinessOutcomeReady {
		t.Fatalf("assessment = %+v", assessment)
	}
	if err := assessment.Validate(source, request, config); err != nil {
		t.Fatalf("the sealed assessment must revalidate: %v", err)
	}
}

// TestAReadinessAnswerWithANeededFieldOfTheWrongTypeStillFailsTheTurn is the
// other side of the boundary: shape is forgiven, a value the engine cannot
// read is not.
func TestAReadinessAnswerWithANeededFieldOfTheWrongTypeStillFailsTheTurn(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"decision":["ready"],"questions":[],"assumptions":[],"reject_code":""}`,
	)})

	if _, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil); err == nil {
		t.Fatal("a decision that is not text was accepted")
	}
}

// TestAReadinessCheckWithExtraFieldsIsRead covers the checker.
func TestAReadinessCheckWithExtraFieldsIsRead(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	assessor := &scriptedChatAPI{responses: []scriptedResponse{
		{text: `{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`, requestID: "request-assess"},
	}}
	invoker, _ := NewModelInvoker(assessor)
	assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}

	checkerAPI := &scriptedChatAPI{responses: []scriptedResponse{
		{text: `{"verdict":"pass","reasons":[],"request_kind":"change","needs_design":true,"severity":"none"}`, requestID: "request-check"},
	}}
	checker, _ := NewModelInvoker(checkerAPI)
	check, _, err := checker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config, nil)
	if err != nil {
		t.Fatalf("a check carrying an extra key must be read: %v", err)
	}
	if check.Verdict != "pass" {
		t.Fatalf("check = %+v", check)
	}
	if err := check.Validate(assessment, source, request, config); err != nil {
		t.Fatalf("the sealed check must revalidate: %v", err)
	}
}

// TestAReviewAnswerWithAnExtraFieldIsRead covers the candidate reviewers, the
// seats that decide whether a round converges. A verdict thrown away for a
// key nobody reads costs the round it was given in.
func TestAReviewAnswerWithAnExtraFieldIsRead(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"verdict":"pass","findings":[],"confidence":"high"}`,
	)})

	review, _, err := invoker.ReviewCandidate(context.Background(), config.Models.Reviewers[1], candidate, nil, source, request, config)
	if err != nil {
		t.Fatalf("a review carrying an extra key must be read: %v", err)
	}
	if review.Verdict != "pass" {
		t.Fatalf("review = %+v", review)
	}
}

// TestAReviewWithAVerdictOfTheWrongTypeStillFailsTheTurn keeps the boundary
// on the review path too.
func TestAReviewWithAVerdictOfTheWrongTypeStillFailsTheTurn(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(
		`{"verdict":{"is":"pass"},"findings":[]}`,
	)})

	if _, _, err := invoker.ReviewCandidate(context.Background(), config.Models.Reviewers[1], candidate, nil, source, request, config); err == nil {
		t.Fatal("a verdict that is not text was accepted")
	}
}

// TestACandidateAnswerWithAnExtraFieldIsRead covers the implementer's own
// answer: a change thrown away for a key nobody reads is a whole round.
func TestACandidateAnswerWithAnExtraFieldIsRead(t *testing.T) {
	output, err := DecodeModelCandidateOutput([]byte(
		`{"files":[{"path":"client/src/components/Example.tsx","content":"x","encoding":"utf-8"}],"rationale":"r","tokens_used":120}`,
	))
	if err != nil {
		t.Fatalf("a candidate carrying extra keys must be read: %v", err)
	}
	if len(output.Files) != 1 || output.Files[0].Path != "client/src/components/Example.tsx" || output.Rationale != "r" {
		t.Fatalf("output = %+v", output)
	}
	if _, err := DecodeModelCandidateOutput([]byte(`{"files":"none","rationale":"r"}`)); err == nil {
		t.Fatal("a files field that is not a list was accepted")
	}
}

// TestAnArbitrationAnswerWithAnExtraFieldIsRead covers the seat that breaks a
// deadlock. A ruling thrown away leaves the round stalled with nothing
// decided.
func TestAnArbitrationAnswerWithAnExtraFieldIsRead(t *testing.T) {
	var decoded ModelArbitrationOutput
	if err := decodeModelJSON([]byte(
		`{"ruling":"instruct_implementer","instruction":"Satisfy the acceptance condition.","overruled":[],"statement":"s","evidence":"e","certainty":0.7}`,
	), &decoded); err != nil {
		t.Fatalf("a ruling carrying an extra key must be read: %v", err)
	}
	if decoded.Ruling != RulingInstructImplementer || decoded.Instruction == "" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// TestAnInvestigationTurnWithAnExtraFieldIsRead covers the investigating
// designer's turn loop, which reads one object per turn.
func TestAnInvestigationTurnWithAnExtraFieldIsRead(t *testing.T) {
	answer, err := decodeTurnAnswer([]byte(`{"read":{"id":"m-0001","offset":0},"thinking":"I need the window"}`), ModeInvestigation)
	if err != nil {
		t.Fatalf("a turn carrying an extra key must be read: %v", err)
	}
	if answer.Read == nil || answer.Read.ID != "m-0001" {
		t.Fatalf("answer = %+v", answer)
	}
	// The rule about what a turn may carry is unchanged: exactly one part.
	if _, err := decodeTurnAnswer([]byte(`{"read":{"id":"m-0001","offset":0},"probe":{}}`), ModeInvestigation); err == nil {
		t.Fatal("a turn carrying two parts was accepted")
	}
}

// strictDecodeCallers is every file in this package that calls the strict
// decoder, with what it reads there. A model's answer is not on the list and
// never can be: the distinction between reading a record and reading an
// answer is the whole of this change, and a call that drifts back is how the
// strictness returns one site at a time.
//
// Written as the exact set rather than as a list of exceptions, so both
// directions are pinned: a new caller fails here, and a caller that went away
// fails here too rather than leaving a licence nobody needs.
var strictDecodeCallers = map[string]string{
	// The fixture inventory this package's own tests load: a record in the
	// repository, not an answer.
	"readiness_fixtures_test.go": "the fixture file the readiness tests load",
	// Tests that assert the two decoders' own behaviour, using an accept
	// function that stands in for a caller's.
	"model_test.go":           "the record path, in the test about a key written twice",
	"model_objection_test.go": "an accept function standing in for a caller's, in a test about the objection",
}

// TestNoModelAnswerIsReadStrictly walks the package's own source. A call to
// the strict decoder outside the set above is a model answer being held to a
// record's rules again.
func TestNoModelAnswerIsReadStrictly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var callers []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strictDecodeCalls(t, entry.Name()) > 0 {
			callers = append(callers, entry.Name())
		}
	}
	sort.Strings(callers)
	expected := make([]string, 0, len(strictDecodeCallers))
	for name := range strictDecodeCallers {
		expected = append(expected, name)
	}
	sort.Strings(expected)
	if strings.Join(callers, ", ") != strings.Join(expected, ", ") {
		t.Fatalf("the strict decoder is called from %s; it is called from %s in this change.\n"+
			"A model's answer is read with decodeModelJSON. If a new call reads a record this package "+
			"wrote or configuration a person wrote, add the file to strictDecodeCallers with what it reads there.",
			strings.Join(callers, ", "), strings.Join(expected, ", "))
	}
}

func strictDecodeCalls(t *testing.T, filename string) int {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", filename), nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", filename, err)
	}
	calls := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		call, isCall := node.(*ast.CallExpr)
		if !isCall {
			return true
		}
		if name, isName := call.Fun.(*ast.Ident); isName && name.Name == "decodeStrictJSON" {
			calls++
		}
		return true
	})
	return calls
}
