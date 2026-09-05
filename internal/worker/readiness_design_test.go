package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
)

// The reception's design decision (README / issue #18 §6): a change request
// skips the design stage only when the ticket itself states the approach
// (quoted, and the quote really is in the ticket), the derived target files
// are two or fewer, the destination configured a trigger vocabulary and none
// of it appears, the request is a change - and neither AI kept the design.

const (
	designApproachBody  = "Replace the visible label. How to do it: change the label constant in Example.tsx to the new wording."
	designApproachQuote = "change the label constant in Example.tsx to the new wording"
)

func designTriggerWords() []string {
	return []string{"slow", "intermittent", "in production", "logs", "root cause", "investigate"}
}

func boolPtr(value bool) *bool { return &value }

// designFixture builds a ticket with the given body and target files, bound
// to a configuration carrying the given design policy (nil = absent).
func designFixture(t *testing.T, design *DesignConfig, body string, files ...string) (Config, TicketRequest, SourceSnapshot) {
	t.Helper()
	config := validTestConfig()
	config.Consumers[0].Design = design
	if len(files) == 0 {
		files = []string{"client/src/components/Example.tsx"}
	}
	lines := []string{"Automation-Run-ID: run_20260802_alpha", "Automation-Mode: client-visible-change"}
	for _, file := range files {
		lines = append(lines, "Target-File: "+file)
	}
	lines = append(lines, "Verification-Path: /settings", "Expected-Text: Updated label", "Absent-Text: Old label", "---", body)
	request, err := ParseTicket(validTicketEnvelope(t, strings.Join(lines, "\n")), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, file := range files {
		filename := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	return config, request, source
}

// designOutput is a ready proposer answer with the design half filled in.
func designOutput(kind string, approachInTicket bool, excerpt string, needsDesign bool) ModelReadinessOutput {
	output := testReadyOutput()
	output.RequestKind, output.ApproachInTicket, output.ApproachExcerpt, output.NeedsDesign = kind, approachInTicket, excerpt, boolPtr(needsDesign)
	return output
}

func skipOutput() ModelReadinessOutput {
	return designOutput(RequestKindChange, true, designApproachQuote, false)
}

// designPair seals attempt 1: the proposer's answer and a passing check that
// carries the checker's own re-derivation.
func designPair(t *testing.T, output ModelReadinessOutput, checkKind string, checkNeedsDesign bool, source SourceSnapshot, request TicketRequest, config Config) (ReadinessAssessment, ReadinessCheck) {
	t.Helper()
	assessment, err := NewReadinessAssessment(1, output, nil, nil, source, request, config, validTestInvocation(config.Models.Readiness.Assessor), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	check, err := NewReadinessCheck(ModelReadinessCheckOutput{
		Verdict: "pass", Reasons: []ReadinessCheckReason{}, RequestKind: checkKind, NeedsDesign: boolPtr(checkNeedsDesign),
	}, assessment, source, request, config, validTestInvocation(config.Models.Readiness.Checker), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return assessment, check
}

func designDecision(t *testing.T, output ModelReadinessOutput, checkKind string, checkNeedsDesign bool, source SourceSnapshot, request TicketRequest, config Config) ReadinessDecision {
	t.Helper()
	assessment, check := designPair(t, output, checkKind, checkNeedsDesign, source, request, config)
	decision, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := decision.Validate([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config); err != nil {
		t.Fatalf("the sealed decision does not re-derive: %v", err)
	}
	return decision
}

// An empty or absent trigger vocabulary fails its condition: the framework
// holds no default list, so "no words configured" must mean "no skip", not
// "nothing to trigger on". The control run differs only in the vocabulary.
func TestEmptyTriggerWordsNeverSkipDesign(t *testing.T) {
	for name, design := range map[string]*DesignConfig{
		"absent":     nil,
		"empty list": {Default: DesignDefaultOn, TriggerWords: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			config, request, source := designFixture(t, design, designApproachBody)
			decision := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config)
			if !decision.NeedsDesign || decision.DesignReason != DesignReasonTriggerWordsUnset {
				t.Fatalf("decision = (%v, %q), want design kept for the unset vocabulary", decision.NeedsDesign, decision.DesignReason)
			}
			if !decision.ApproachInTicket || decision.ApproachExcerpt != designApproachQuote || decision.RequestKind != RequestKindChange {
				t.Fatalf("the other conditions were not recorded: %+v", decision)
			}
		})
	}
	config, request, source := designFixture(t, &DesignConfig{TriggerWords: designTriggerWords()}, designApproachBody)
	control := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config)
	if control.NeedsDesign || control.DesignReason != DesignReasonApproachInTicket {
		t.Fatalf("control decision = (%v, %q), want the design skipped", control.NeedsDesign, control.DesignReason)
	}
}

// The two-AI rule: the proposer's needs_design and the checker's independent
// one must both say "skip" for the design to be skipped. Either voice alone
// keeps it, an unanswered checker counts as a voice for it, and the sealed
// decision cannot be edited to the skip afterwards.
func TestNeedsDesignFallsToSafeSide(t *testing.T) {
	config, request, source := designFixture(t, &DesignConfig{TriggerWords: designTriggerWords()}, designApproachBody)

	agreed := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config)
	if agreed.NeedsDesign || agreed.DesignReason != DesignReasonApproachInTicket {
		t.Fatalf("both said skip, decision = (%v, %q)", agreed.NeedsDesign, agreed.DesignReason)
	}

	checkerObjects := designDecision(t, skipOutput(), RequestKindChange, true, source, request, config)
	if !checkerObjects.NeedsDesign || checkerObjects.DesignReason != DesignReasonChecker {
		t.Fatalf("checker objected, decision = (%v, %q)", checkerObjects.NeedsDesign, checkerObjects.DesignReason)
	}

	proposerObjects := designDecision(t, designOutput(RequestKindChange, true, designApproachQuote, true), RequestKindChange, false, source, request, config)
	if !proposerObjects.NeedsDesign || proposerObjects.DesignReason != DesignReasonProposer {
		t.Fatalf("proposer objected, decision = (%v, %q)", proposerObjects.NeedsDesign, proposerObjects.DesignReason)
	}

	// A checker that never answers needs_design is read as keeping the design.
	assessment, err := NewReadinessAssessment(1, skipOutput(), nil, nil, source, request, config, validTestInvocation(config.Models.Readiness.Assessor), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(`{"verdict":"pass","reasons":[]}`)})
	silentCheck, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config)
	if err != nil {
		t.Fatalf("an unanswered needs_design killed the check: %v", err)
	}
	if !silentCheck.NeedsDesign || silentCheck.RequestKind != RequestKindChange {
		t.Fatalf("silent check = (%v, %q)", silentCheck.NeedsDesign, silentCheck.RequestKind)
	}
	silent, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{silentCheck}, source, request, config)
	if err != nil || !silent.NeedsDesign || silent.DesignReason != DesignReasonChecker {
		t.Fatalf("silent checker, decision = (%v, %q), error = %v", silent.NeedsDesign, silent.DesignReason, err)
	}

	// The kind, too: an investigation needs both voices; a lone one is a
	// change. And the voice that called it an investigation never judged the
	// change - its sealed needs_design false is the investigation
	// short-circuit - so it counts as keeping the design: with every
	// mechanical condition met, the other voice alone must not skip it.
	loneInvestigation := designDecision(t, designOutput(RequestKindInvestigation, false, "", false), RequestKindChange, true, source, request, config)
	if loneInvestigation.RequestKind != RequestKindChange || !loneInvestigation.NeedsDesign || loneInvestigation.DesignReason != DesignReasonApproachMissing {
		t.Fatalf("lone investigation, decision = %+v", loneInvestigation)
	}
	proposerInvestigation := designDecision(t, designOutput(RequestKindInvestigation, true, designApproachQuote, false), RequestKindChange, false, source, request, config)
	if proposerInvestigation.RequestKind != RequestKindChange || !proposerInvestigation.NeedsDesign || proposerInvestigation.DesignReason != DesignReasonProposer {
		t.Fatalf("proposer said investigation with a quote, checker said skip: decision = %+v", proposerInvestigation)
	}
	loneChecker := designDecision(t, skipOutput(), RequestKindInvestigation, false, source, request, config)
	if loneChecker.RequestKind != RequestKindChange || !loneChecker.NeedsDesign || loneChecker.DesignReason != DesignReasonChecker {
		t.Fatalf("proposer said skip, checker said investigation: decision = %+v", loneChecker)
	}

	// Editing the sealed decision to a skip fails both the gate check and the
	// full re-derivation, even with the digest recomputed.
	tampered := checkerObjects
	tampered.NeedsDesign, tampered.DesignReason = false, DesignReasonApproachInTicket
	digest, err := readinessDecisionDigest(tampered)
	if err != nil {
		t.Fatal(err)
	}
	tampered.DecisionSHA256 = digest
	if err := tampered.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("ValidateBinding() cannot tell a skip both mechanical conditions allow: %v", err)
	}
	assessment2, check2 := designPair(t, skipOutput(), RequestKindChange, true, source, request, config)
	if err := tampered.Validate([]ReadinessAssessment{assessment2}, []ReadinessCheck{check2}, source, request, config); err == nil {
		t.Fatal("Validate() accepted a decision whose checker objection was edited out")
	}
}

// A quote is evidence only when it is really in the ticket: a paraphrase, an
// empty quote, or a claim without a quote leaves the approach unproven and
// the design kept. Re-flowed whitespace is not a difference.
func TestApproachExcerptMustAppearInTheTicket(t *testing.T) {
	config, request, source := designFixture(t, &DesignConfig{TriggerWords: designTriggerWords()}, designApproachBody)
	seal := func(output ModelReadinessOutput) ReadinessAssessment {
		t.Helper()
		assessment, err := NewReadinessAssessment(1, output, nil, nil, source, request, config, validTestInvocation(config.Models.Readiness.Assessor), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		return assessment
	}
	for name, output := range map[string]ModelReadinessOutput{
		"paraphrase":          designOutput(RequestKindChange, true, "update the constant that holds the label", false),
		"empty quote":         designOutput(RequestKindChange, true, "", false),
		"quote without claim": designOutput(RequestKindChange, false, designApproachQuote, false),
		"text from elsewhere": designOutput(RequestKindChange, true, "export const label = 'Old label';", false),
		// A fragment is in the ticket but is no statement of an approach.
		"one letter":       designOutput(RequestKindChange, true, "a", false),
		"one word":         designOutput(RequestKindChange, true, "the", false),
		"eleven runes":     designOutput(RequestKindChange, true, "change the ", false),
		"the title alone":  designOutput(RequestKindChange, true, request.Summary, false),
		"the title spaced": designOutput(RequestKindChange, true, "  Change  one visible\nlabel ", false),
	} {
		t.Run(name, func(t *testing.T) {
			assessment := seal(output)
			if assessment.ApproachInTicket || assessment.ApproachExcerpt != "" || !assessment.NeedsDesign || assessment.DesignReason != DesignReasonApproachMissing {
				t.Fatalf("assessment design = %+v", assessment)
			}
		})
	}
	reflowed := seal(designOutput(RequestKindChange, true, " change the label constant\n  in Example.tsx to the new wording ", false))
	if !reflowed.ApproachInTicket || reflowed.NeedsDesign || reflowed.ApproachExcerpt != "change the label constant\n  in Example.tsx to the new wording" {
		t.Fatalf("re-flowed quote was not recognised: %+v", reflowed)
	}
	// The length floor is a floor: twelve runes of the body pass it (the
	// meaning of a quote is the two models' judgment, not the engine's).
	floor := seal(designOutput(RequestKindChange, true, "change the l", false))
	if !floor.ApproachInTicket || floor.NeedsDesign {
		t.Fatalf("a quote on the length floor was refused: %+v", floor)
	}
	// A sealed assessment whose quote is not in the ticket is refused even
	// with its digest recomputed.
	forged := reflowed
	forged.ApproachExcerpt = "update the constant that holds the label"
	digest, err := readinessAssessmentDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.AssessmentSHA256 = digest
	if err := forged.Validate(source, request, config); err == nil || !strings.Contains(err.Error(), "not in the ticket") {
		t.Fatalf("Validate() error = %v, want the quote refused", err)
	}
}

// An investigation has no design: both AIs call it one, and the decision says
// so regardless of the destination's policy or the other conditions.
func TestInvestigationRequestsCarryNoDesign(t *testing.T) {
	body := "Find out why the settings screen sometimes shows the old label after saving. Do not change anything yet."
	for name, design := range map[string]*DesignConfig{
		"default on, no words": nil,
		"default off":          {Default: DesignDefaultOff},
		"words configured":     {TriggerWords: designTriggerWords()},
	} {
		t.Run(name, func(t *testing.T) {
			config, request, source := designFixture(t, design, body)
			decision := designDecision(t, designOutput(RequestKindInvestigation, false, "", false), RequestKindInvestigation, false, source, request, config)
			if decision.RequestKind != RequestKindInvestigation || decision.NeedsDesign || decision.DesignReason != DesignReasonInvestigation {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

// design.default off skips the design for every change request, whatever the
// four conditions and the two AIs say; the reason names the setting.
func TestDesignDefaultOffSkipsDesignForChangeRequests(t *testing.T) {
	config, request, source := designFixture(t, &DesignConfig{Default: DesignDefaultOff}, "Replace the visible label; it is slow in production.")
	decision := designDecision(t, designOutput(RequestKindChange, false, "", true), RequestKindChange, true, source, request, config)
	if decision.NeedsDesign || decision.DesignReason != DesignReasonDefaultOff || decision.RequestKind != RequestKindChange {
		t.Fatalf("decision = %+v", decision)
	}
}

// The remaining mechanical conditions, each on its own with the others held
// satisfied: more than two derived target files, and a configured trigger
// word in the ticket (matched without regard to case).
func TestTargetFilesAndTriggerWordsKeepTheDesign(t *testing.T) {
	words := &DesignConfig{TriggerWords: designTriggerWords()}
	three := []string{"client/src/components/Example.tsx", "client/src/components/Other.tsx", "client/src/components/Third.tsx"}
	config, request, source := designFixture(t, words, designApproachBody, three...)
	if decision := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config); !decision.NeedsDesign || decision.DesignReason != DesignReasonTooManyFiles {
		t.Fatalf("three files, decision = (%v, %q)", decision.NeedsDesign, decision.DesignReason)
	}
	config, request, source = designFixture(t, words, designApproachBody, three[:2]...)
	if decision := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config); decision.NeedsDesign {
		t.Fatalf("two files, decision = (%v, %q)", decision.NeedsDesign, decision.DesignReason)
	}
	config, request, source = designFixture(t, words, designApproachBody+" It renders wrongly In Production only.")
	if decision := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config); !decision.NeedsDesign || decision.DesignReason != DesignReasonTriggerWord {
		t.Fatalf("trigger word, decision = (%v, %q)", decision.NeedsDesign, decision.DesignReason)
	}
}

// The model path coerces, never objects: answers without the design fields,
// or with an unknown kind, still seal - on the side that keeps the design.
func TestAssessReadinessCoercesUnansweredDesignFields(t *testing.T) {
	config, request, source := designFixture(t, &DesignConfig{TriggerWords: designTriggerWords()}, designApproachBody)
	for name, answer := range map[string]string{
		"fields absent": `{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`,
		"unknown kind":  `{"decision":"ready","questions":[],"assumptions":[],"reject_code":"","request_kind":"bogus","approach_in_ticket":false,"approach_excerpt":"","needs_design":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			api := &fakeChatAPI{output: chatOutput(answer)}
			invoker, _ := NewModelInvoker(api)
			assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config)
			if err != nil {
				t.Fatalf("the answer was objected to: %v", err)
			}
			if assessment.RequestKind != RequestKindChange || assessment.ApproachInTicket || !assessment.NeedsDesign || assessment.DesignReason != DesignReasonApproachMissing {
				t.Fatalf("assessment design = %+v", assessment)
			}
			if !strings.Contains(api.request.Messages[1].Content, `"design_trigger_words":["slow"`) {
				t.Fatalf("the proposer was not shown the trigger vocabulary:\n%s", api.request.Messages[1].Content)
			}
		})
	}
	// A verified skip through the model path, and the checker shown the
	// proposer's sealed design half.
	api := &fakeChatAPI{output: chatOutput(`{"decision":"ready","questions":[],"assumptions":[],"reject_code":"","request_kind":"change","approach_in_ticket":true,"approach_excerpt":"` + designApproachQuote + `","needs_design":false}`)}
	invoker, _ := NewModelInvoker(api)
	assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, nil, nil, source, request, config)
	if err != nil || assessment.NeedsDesign || !assessment.ApproachInTicket {
		t.Fatalf("assessment = %+v, error = %v", assessment, err)
	}
	checker := &fakeChatAPI{output: chatOutput(`{"verdict":"pass","reasons":[],"request_kind":"change","needs_design":false}`)}
	invoker, _ = NewModelInvoker(checker)
	check, _, err := invoker.CheckReadiness(context.Background(), assessment, nil, nil, source, request, config)
	if err != nil || check.NeedsDesign {
		t.Fatalf("check = %+v, error = %v", check, err)
	}
	if !strings.Contains(checker.request.Messages[1].Content, `"approach_excerpt":"`+designApproachQuote+`"`) || !strings.Contains(checker.request.Messages[1].Content, `"needs_design":false`) {
		t.Fatalf("the checker was not shown the proposer's design half:\n%s", checker.request.Messages[1].Content)
	}
}

// The decision has its own schema version: the shape before the design
// decision is refused at the gate, and every field of the design decision
// is held to the ticket without the chain.
func TestReadinessDecisionSchemaVersionRejectsTheOldShape(t *testing.T) {
	config, request, source := designFixture(t, &DesignConfig{TriggerWords: designTriggerWords()}, designApproachBody)
	decision := designDecision(t, skipOutput(), RequestKindChange, false, source, request, config)
	if decision.SchemaVersion != ReadinessDecisionSchemaVersion {
		t.Fatalf("decision schema version = %d, want %d", decision.SchemaVersion, ReadinessDecisionSchemaVersion)
	}
	if ReadinessDecisionSchemaVersion != 2 {
		t.Fatalf("the design decision's schema version is %d, want 2", ReadinessDecisionSchemaVersion)
	}
	reseal := func(mutate func(*ReadinessDecision)) ReadinessDecision {
		t.Helper()
		edited := decision
		mutate(&edited)
		digest, err := readinessDecisionDigest(edited)
		if err != nil {
			t.Fatal(err)
		}
		edited.DecisionSHA256 = digest
		return edited
	}
	for name, mutate := range map[string]func(*ReadinessDecision){
		"version 1":             func(d *ReadinessDecision) { d.SchemaVersion = 1 },
		"unknown reason":        func(d *ReadinessDecision) { d.DesignReason = "because" },
		"reason without design": func(d *ReadinessDecision) { d.DesignReason = DesignReasonTriggerWord },
		"unknown kind":          func(d *ReadinessDecision) { d.RequestKind = "maybe" },
		"quote edited":          func(d *ReadinessDecision) { d.ApproachExcerpt = "update the constant that holds the label" },
		"quote dropped":         func(d *ReadinessDecision) { d.ApproachExcerpt = "" },
		"skip without approach": func(d *ReadinessDecision) { d.ApproachInTicket, d.ApproachExcerpt = false, "" },
	} {
		t.Run(name, func(t *testing.T) {
			if err := reseal(mutate).ValidateBinding(source, request, config); err == nil {
				t.Fatal("ValidateBinding() accepted the edited decision")
			}
		})
	}
	if err := reseal(func(*ReadinessDecision) {}).ValidateBinding(source, request, config); err != nil {
		t.Fatalf("the unedited decision must still pass: %v", err)
	}
	// The kind is the one thing the ticket alone cannot re-derive - it is what
	// the two AIs said - so a decision edited to "investigation" passes the
	// chainless gate check and is caught by the full re-derivation.
	byFiat := reseal(func(d *ReadinessDecision) {
		d.RequestKind, d.DesignReason = RequestKindInvestigation, DesignReasonInvestigation
	})
	if err := byFiat.ValidateBinding(source, request, config); err != nil {
		t.Fatalf("ValidateBinding() cannot know what the AIs said: %v", err)
	}
	assessment, check := designPair(t, skipOutput(), RequestKindChange, false, source, request, config)
	if err := byFiat.Validate([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config); err == nil {
		t.Fatal("Validate() accepted a kind neither AI gave")
	}
}

// Both prompt contracts carry the design half, and the structured-output
// schemas demand every field of it.
func TestReadinessPromptsCarryTheDesignContract(t *testing.T) {
	if !strings.Contains(readinessJSONSchema(), `"required":["decision","questions","assumptions","reject_code","request_kind","approach_in_ticket","approach_excerpt","needs_design"]`) {
		t.Fatal("the assessor schema does not require the design fields")
	}
	if !strings.Contains(readinessCheckJSONSchema(), `"required":["verdict","reasons","request_kind","needs_design"]`) {
		t.Fatal("the checker schema does not require the design fields")
	}
	for _, want := range []string{"request_kind", "approach_in_ticket", "approach_excerpt", "needs_design", "design_trigger_words", "at most two of the target_files"} {
		if !strings.Contains(readinessSystemPrompt(), want) {
			t.Fatalf("the assessor prompt lacks %q", want)
		}
	}
	checker := readinessCheckSystemPrompt(validTestConfig().Models.Readiness.Checker)
	for _, want := range []string{"request_kind", "needs_design", "independent re-derivation", "design_trigger_words"} {
		if !strings.Contains(checker, want) {
			t.Fatalf("the checker prompt lacks %q", want)
		}
	}
	if readinessPromptVersion != 9 {
		t.Fatalf("prompt version = %d, want the design contract's 9", readinessPromptVersion)
	}
}

// Every reason the engine can seal has a sentence on the ticket, and agrees
// with itself on whether it keeps the design.
func TestEveryDesignReasonHasARequesterPhrase(t *testing.T) {
	for _, reason := range DesignReasons {
		if _, known := hook.DesignReasonPhrase(reason); !known {
			t.Errorf("design reason %q has no requester phrase", reason)
		}
		if _, known := DesignReasonKeepsDesign(reason); !known {
			t.Errorf("design reason %q is not classified", reason)
		}
	}
	if keeps, _ := DesignReasonKeepsDesign(DesignReasonApproachInTicket); keeps {
		t.Fatal("approach_in_ticket must mean the design is skipped")
	}
	if keeps, _ := DesignReasonKeepsDesign(DesignReasonChecker); !keeps {
		t.Fatal("checker_disagreed must mean the design is kept")
	}
	if _, known := DesignReasonKeepsDesign("because"); known {
		t.Fatal("an unknown reason was classified")
	}
}
