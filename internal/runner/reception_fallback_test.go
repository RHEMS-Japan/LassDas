package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

// unusableAnswerStderr is what the worker prints when every answer of a turn
// arrived and none could be used: the failure line the runner reads the cause
// from, and the evidence line that names the class in the worker's own words.
func unusableAnswerStderr(t *testing.T) string {
	t.Helper()
	detail, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.AnswerUnusablePhrase, Model: "model-a", MaxOutputTokens: 4096,
		Calls: 3, Malformed: 3, LastRequestID: "request-123", LastFinishReason: worker.ChatFinishStop,
		Objection: "the answer carries no JSON value (answer 3 of 3)",
	})
	if err != nil {
		t.Fatal(err)
	}
	return "worker: readiness assessment failed: " + worker.AnswerUnusablePhrase + "\\n" +
		worker.FailureDetailLinePrefix + string(detail)
}

// fallbackStubWorker fails the named subcommand with the given stderr and
// succeeds at everything else, writing the file named by --out when one is
// asked for. The gate's own record has to appear for the run to go on.
func fallbackStubWorker(t *testing.T, failing, stderr string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"" + failing + "\" ]; then printf '%b\\n' '" + stderr + "' >&2; exit 1; fi\n" +
		"verb=\"$1\"; shift\n" +
		"out=\"\"\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = \"--out\" ]; then out=\"$2\"; fi; shift; done\n" +
		"if [ \"$verb\" = \"decide-readiness-without-readers\" ] && [ -n \"$out\" ]; then\n" +
		"  printf '%s' '{\"outcome\":\"ready\",\"request_kind\":\"change\",\"needs_design\":false,\"design_reason\":\"reception_unread\",\"fallback\":true}' > \"$out\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// TestAReceptionReaderThatAnswersNothingUsableDoesNotEndTheRun is the whole
// point of the path: the gate is decided without the reader and the delivery
// goes on to the implementer, instead of ending as a model failure two
// minutes after it started.
func TestAReceptionReaderThatAnswersNothingUsableDoesNotEndTheRun(t *testing.T) {
	stub := fallbackStubWorker(t, "assess-readiness", unusableAnswerStderr(t))
	pipeline := receptionPipeline(t, stub)

	outcome, err := pipeline.readinessGate(context.Background())
	if err != nil || outcome.Code != "" {
		t.Fatalf("readinessGate() = %+v, %v; want the run to go on", outcome, err)
	}
	decision := pipeline.path("history/readiness/decision.json")
	raw, readErr := os.ReadFile(decision)
	if readErr != nil {
		t.Fatalf("the gate decided without its readers left no record: %v", readErr)
	}
	var sealed struct {
		Outcome      string `json:"outcome"`
		RequestKind  string `json:"request_kind"`
		NeedsDesign  bool   `json:"needs_design"`
		DesignReason string `json:"design_reason"`
		Fallback     bool   `json:"fallback"`
	}
	if err := json.Unmarshal(raw, &sealed); err != nil {
		t.Fatal(err)
	}
	if sealed.Outcome != "ready" || sealed.RequestKind != "change" || sealed.NeedsDesign ||
		sealed.DesignReason != "reception_unread" || !sealed.Fallback {
		t.Fatalf("the gate decided without its readers = %+v", sealed)
	}
	// No trail: the reception did not fail the requester, it read what it
	// could. A trail would be attached to a report that is never made.
	if _, err := os.Stat(pipeline.path("m1-trail.txt")); !os.IsNotExist(err) {
		t.Fatalf("a failure trail was written for a reception that went on: %v", err)
	}
}

// TestTheRequesterIsToldOnceThatTheReceptionWasReadWithoutTheModel: the line
// goes in the stream both the plan notice and the closing comment read, and
// goes in once however many halves of the reception had to be read this way.
func TestTheRequesterIsToldOnceThatTheReceptionWasReadWithoutTheModel(t *testing.T) {
	stub := fallbackStubWorker(t, "assess-readiness", unusableAnswerStderr(t))
	pipeline := receptionPipeline(t, stub)
	if outcome, err := pipeline.readinessGate(context.Background()); err != nil || outcome.Code != "" {
		t.Fatalf("readinessGate() = %+v, %v", outcome, err)
	}

	// The intake's own fallback records the same line, and the delivery must
	// carry one, not two.
	pipeline.recordReceptionFallback()

	recorded := LoadRecordedDecisions(pipeline.Workspace)
	if len(recorded) != 1 {
		t.Fatalf("recorded decisions = %+v, want exactly one", recorded)
	}
	statement := recorded[0].Statement
	for _, want := range []string{"受付の読み取り役", "チケットの本文", "質問はしていません"} {
		if !strings.Contains(statement, want) {
			t.Fatalf("the line does not say what happened (%q missing): %q", want, statement)
		}
	}
	// Not a point the requester is invited to take back: nothing was decided
	// on their behalf about the request itself.
	if recorded[0].Decided {
		t.Fatalf("the line was filed as a point decided in place of asking: %+v", recorded[0])
	}
	// The line names nothing from inside the engine.
	for _, leaked := range []string{"JSON", "decode", "intake.json", "model", "fallback"} {
		if strings.Contains(statement, leaked) {
			t.Fatalf("the line names %q, which is not the requester's word for anything: %q", leaked, statement)
		}
	}
}

// TestAReceptionFailureThatIsNotAnAnswerStillEndsTheRun is the boundary. A
// key with nothing left to spend, a provider that refused, a volume with no
// room: each is an instance an operator has to change, and a gate decided
// without readers would hide it behind a delivery that went ahead.
func TestAReceptionFailureThatIsNotAnAnswerStillEndsTheRun(t *testing.T) {
	stderrs := map[string]string{
		"an allowance that is spent": "worker: readiness assessment failed: " + worker.TransportFailedPhrase + ": " + worker.SpentAllowancePhrase,
		"an answer cut off":          "worker: readiness assessment failed: model response ended before a complete answer: finish_reason=length",
		"no evidence line at all":    "worker: readiness assessment failed: derived contract is invalid",
	}
	for name, stderr := range stderrs {
		stub := fallbackStubWorker(t, "assess-readiness", stderr)
		pipeline := receptionPipeline(t, stub)
		outcome, err := pipeline.readinessGate(context.Background())
		if outcome.Code != hook.TerminalModelFailed {
			t.Errorf("%s: readinessGate() = %+v, %v; want model_failed", name, outcome, err)
		}
		if _, statErr := os.Stat(pipeline.path("history/readiness/decision.json")); !os.IsNotExist(statErr) {
			t.Errorf("%s: a gate was decided without readers for a failure that was not an answer", name)
		}
	}
}

// TestAGateThatCannotBeSealedEndsTheRun: the delivery goes on past the
// reception only when there is a record saying it may. A seal that failed
// leaves none, and the run ends the honest way rather than proceeding on
// nothing.
func TestAGateThatCannotBeSealedEndsTheRun(t *testing.T) {
	script := filepath.Join(t.TempDir(), "stand-in-worker")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"assess-readiness\" ]; then printf '%b\\n' '" + unusableAnswerStderr(t) + "' >&2; exit 1; fi\n" +
		"if [ \"$1\" = \"decide-readiness-without-readers\" ]; then exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	pipeline := receptionPipeline(t, script)

	outcome, err := pipeline.readinessGate(context.Background())
	if outcome.Code != hook.TerminalModelFailed {
		t.Fatalf("readinessGate() = %+v, %v; want model_failed", outcome, err)
	}
}
