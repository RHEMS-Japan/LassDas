package runner

import (
	"encoding/json"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// A chain stage is not the reception: it sits on the ladder, so an answer
// nobody can use has somewhere to go. This pins the class rather than an
// ending, because the class is what the ladder reads — another seat for the
// role, then a shorter ask of the same one, then a growing wait — and the
// ending is exactly what must not happen.
//
// The pin matters because the tolerant reading changed what reaches here: an
// answer with a key nobody reads no longer counts as malformed at all, so the
// only failures left on this path are answers the engine genuinely cannot
// use, and those must still be read as the seat's.
func TestAnAnswerNobodyCanUseIsTheSeatsFailureNotTheRunsEnding(t *testing.T) {
	encoded, err := json.Marshal(worker.ModelFailureDetail{
		Phrase: worker.AnswerUnusablePhrase, Model: "model-b", MaxOutputTokens: 4096,
		Calls: 3, Malformed: 3, LastRequestID: "request-9", LastFinishReason: worker.ChatFinishStop,
		Objection: "the answer carries no JSON value (answer 3 of 3, request request-9)",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"agent-review", "run-instruction", "arbitrate", "investigate"} {
		stderr := "worker: the " + verb + " step could not be completed: " + worker.AnswerUnusablePhrase + "\n" +
			worker.FailureDetailLinePrefix + string(encoded) + "\n"
		detail, spoke := worker.ParseFailureDetailLine(stderr)
		if !spoke || detail.Malformed != 3 {
			t.Fatalf("%s: the worker's account of the turn was not read; this test measures nothing", verb)
		}
		if class := classifyStageFailure(&verbFailure{verb: verb, code: 1, stderr: stderr}); class != FailureClassModel {
			t.Errorf("%s: class = %q, want the seat's own failure so the ladder is consulted", verb, class)
		}
	}
}

// The head of the answer travels with the failure and must not choose the
// class: a ticket whose own words say "no space left on device" would
// otherwise send the ladder after a full volume.
func TestAnUnusableAnswerChoosesTheClassWithoutItsOwnWords(t *testing.T) {
	stderr := "worker: the review could not be completed: " + worker.AnswerUnusablePhrase +
		" (answer 3 of 3, request req-1" + worker.AnswerHeadMarker + "no space left on device, connection refused)\n"
	if left := withoutModelWords(stderr); strings.Contains(left, "no space left") {
		t.Fatalf("the answer's own words survived the strip: %q", left)
	}
	if class := classifyStageFailure(&verbFailure{verb: "agent-review", code: 1, stderr: stderr}); class != FailureClassModel {
		t.Fatalf("class = %q, want the seat's own failure", class)
	}
}
