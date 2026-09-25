package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// The two readings of one line had to be the same reading.
//
// The parser recognises the worker's account of a turn after trimming the
// line, and the strip recognised it only at the very start of one. An
// account written with a leading space therefore parsed as evidence and
// survived the strip, which left its prose — the head of the model's last
// answer among it — in the text the classes are read out of. That is the
// one text a model's answer must never be able to reach.
func TestAnIndentedEvidenceLineIsStrippedLikeAnyOther(t *testing.T) {
	encoded, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.AnswerUnusablePhrase, Model: "m", Calls: 3, Malformed: 3, LastHTTPStatus: 200,
		Objection: "model response content is invalid (answer 3 of 3, request req-1, " +
			"began: the credit balance branch in billing.go is wrong)",
	})
	if err != nil {
		t.Fatal(err)
	}
	line := " " + worker.FailureDetailLinePrefix + string(encoded) + "\n"
	if _, spoke := worker.ParseFailureDetailLine(line); !spoke {
		t.Fatal("the parser did not read the indented line; this test is measuring the wrong thing")
	}
	if left := withoutModelWords(line); strings.Contains(left, "credit") {
		t.Fatalf("the indented account survived the strip: %q", left)
	}
	if class := classifyStageFailure(&verbFailure{verb: "agent-review", code: 1, stderr: line}); class != FailureClassModel {
		t.Fatalf("class = %q, want the model failure the parsed fields describe", class)
	}
}

// The head of a model's answer has a second way out: an error message
// carries it too, and that message reaches the stage's own stderr as an
// ordinary line rather than as the account of a turn. So every line is cut
// at the marker, and what stands before it — this engine's words, an
// attempt count, a request id — is kept.
func TestTheHeadOfAnAnswerNeverReachesTheClassifier(t *testing.T) {
	for _, head := range []string{
		"the credit balance check in billing.go is wrong",
		"connection refused is what the tests print here",
		"no space left on device, said the fixture",
	} {
		stderr := "worker: the review could not be completed: model response content is invalid " +
			"(answer 3 of 3, request req-1" + worker.AnswerHeadMarker + head + ")\n"
		if left := withoutModelWords(stderr); strings.Contains(left, head) {
			t.Fatalf("the answer's own words survived: %q", left)
		}
		if !strings.Contains(withoutModelWords(stderr), "request req-1") {
			t.Fatal("the cut took the engine's own words with it")
		}
		failure := &verbFailure{verb: "agent-review", code: 1, stderr: stderr}
		if class := classifyStageFailure(failure); class != FailureClassModel {
			t.Fatalf("an answer beginning %q chose the class %q", head, class)
		}
	}
}

// The tail a step leaves behind has to be bigger than one whole account of
// a turn. Kept in a buffer no bigger, the longest account arrives with its
// prefix cut away: no longer parseable as evidence, and still there as
// text for the classes to be read out of.
func TestTheStepTailHoldsAWholeEvidenceLine(t *testing.T) {
	if stepStderrTailBytes <= worker.MaxFailureDetailLineBytes {
		t.Fatalf("the step tail is %d bytes and one evidence line may be %d", stepStderrTailBytes, worker.MaxFailureDetailLineBytes)
	}
	// A long account, at the size the worker will actually emit, with the
	// refusal sentence after it: both fit, so the parser still finds the
	// line where a reader keeps only the tail.
	encoded, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.AnswerUnusablePhrase, Model: "m", Calls: 3, Malformed: 3, LastHTTPStatus: 200,
		Objection: strings.Repeat("o", 380),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream := strings.Repeat("noise from the step\n", 512) +
		worker.FailureDetailLinePrefix + string(encoded) + "\nworker: the review could not be completed\n"
	tail := &tailBuffer{limit: stepStderrTailBytes}
	if _, err := tail.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if _, spoke := worker.ParseFailureDetailLine(tail.String()); !spoke {
		t.Fatal("the account did not survive the tail the step keeps")
	}
}
