package attendant

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A round whose sealed review cannot be read used to end the delivery, on
// a code that said the machinery had broken. Nothing about the request had
// been decided: one seat left no usable answer, which is the same thing as
// a model that would not answer and has the same remedy.
//
// So the seat moves to its next candidate and the review is asked again
// for the same round. The delivery does not end, the requester is told
// nothing, and the decision that stood on the missing evidence is gone —
// without that last part the card the tick rebuilds would run against the
// round after this one, which has no change in it yet.
func TestAnUnreadableReviewMovesThatSeatAndAsksAgain(t *testing.T) {
	for name, unreadable := range map[string]string{
		"a review that will not parse": `{"findings":[`,
		"a review that was removed":    "",
	} {
		config, runDir := designRunConfigWithReviewers(t)
		seatTheConsumerConfig(t, config.ConsumerConfigPath)
		stage1 := filepath.Join(runDir, "history", "stage-1")
		if err := os.MkdirAll(stage1, 0o755); err != nil {
			t.Fatal(err)
		}
		// The round as the validate card leaves it: a sealed revise
		// decision over two sealed reviews, one of which no longer reads.
		writeRoundFile(t, stage1, "decision.json", `{"outcome":"revise"}`)
		writeRoundFile(t, stage1, "review-b.json", `{"verdict":"revise","findings":[{"code":"style","message":"a smaller thing"}]}`)
		if unreadable != "" {
			writeRoundFile(t, stage1, "review-a.json", unreadable)
		}
		writeRoundFile(t, stage1, "review-a-run.json", `{"agent_id":"judge-a"}`)

		hermes, callLog := fakeBoard(t)
		logger := &recordingLogger{}
		err := handleChainFailure(context.Background(), config, quietServices(t), hermes,
			hook.DispatchEnvelope{DeliveryID: "delivery-1", Snapshot: hook.TicketSnapshot{IssueID: 4242}},
			state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, failedValidateBoard(), runtime.StageValidate, logger)
		if err != nil {
			t.Fatalf("%s: the tick failed instead of asking the seat again: %v", name, err)
		}

		// The seat that produced the unreadable record, and no other.
		failure, sealed := runner.ReadStageFailure(runDir, runtime.StageReviewA, 1)
		if !sealed || failure.Class != runner.FailureClassModel {
			t.Fatalf("%s: the unreadable review was not recorded as that seat's own failure: %+v", name, failure)
		}
		if _, other := runner.ReadStageFailure(runDir, runtime.StageReviewB, 1); other {
			t.Fatalf("%s: the seat that answered was blamed too", name)
		}
		// The seat moved, onto the candidate that is not the other seat's
		// vendor, and the record says where it came from.
		seat, moved := runner.ReadSeatRecord(runDir, runtime.StageReviewA, "review-a", 1)
		if !moved || seat.Candidate != 2 || seat.MovedTo.Model != "model-c" {
			t.Fatalf("%s: the seat did not move to the free candidate: %+v", name, seat)
		}
		// The round is asked again rather than abandoned: the unreadable
		// record and the decision built on it are gone, the other seat's
		// review stands, and the review card is back on this round.
		for _, gone := range []string{"review-a.json", "review-a-run.json", "decision.json"} {
			if _, err := os.Stat(filepath.Join(stage1, gone)); !os.IsNotExist(err) {
				t.Fatalf("%s: %s was left behind: %v", name, gone, err)
			}
		}
		if _, err := os.Stat(filepath.Join(stage1, "review-b.json")); err != nil {
			t.Fatalf("%s: the seat that answered lost its review: %v", name, err)
		}
		_, created := boardCalls(t, callLog)
		if !containsID(created, runtime.ChainCardKey("delivery-1", runtime.StageReviewA, 1)) {
			t.Fatalf("%s: the review was not asked again: %v", name, created)
		}
		if reason, _ := os.ReadFile(filepath.Join(runDir, "delivery-stop-reason.txt")); len(reason) > 0 {
			t.Fatalf("%s: the delivery ended anyway: %q", name, reason)
		}
	}
}

// And when the seat has nowhere left to go, it descends like any other
// model failure: the instruction is rebuilt once, and then the delivery
// waits. It never ends, and the judges are never reduced to one.
func TestAnUnreadableReviewDescendsTheLadderWhenTheSeatIsSpent(t *testing.T) {
	config, runDir := designRunConfigWithReviewers(t)
	stage1 := filepath.Join(runDir, "history", "stage-1")
	if err := os.MkdirAll(stage1, 0o755); err != nil {
		t.Fatal(err)
	}
	var steps []int
	for pass := 0; pass < 3; pass++ {
		// Each pass leaves the round as the validate card would have: the
		// review is asked again, and the answer is unreadable again.
		writeRoundFile(t, stage1, "decision.json", `{"outcome":"revise"}`)
		writeRoundFile(t, stage1, "review-a.json", `{"findings":[`)
		writeRoundFile(t, stage1, "review-b.json", `{"verdict":"revise","findings":[{"code":"style","message":"a smaller thing"}]}`)
		hermes, _ := fakeBoard(t)
		err := handleChainFailure(context.Background(), config, quietServices(t), hermes,
			hook.DispatchEnvelope{DeliveryID: "delivery-1", Snapshot: hook.TicketSnapshot{IssueID: 4242}},
			state.RunOverview{DeliveryID: "delivery-1", RunID: "run-1"}, failedValidateBoard(), runtime.StageValidate, &recordingLogger{})
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		steps = append(steps, readLadderRecord(runDir, runtime.StageReviewA, 1).LadderStep)
	}
	record := readLadderRecord(runDir, runtime.StageReviewA, 1)
	if !slices.Equal(record.Tried, []string{"prompt:shorten"}) {
		t.Fatalf("tried = %v, want the one hand a seat with nobody else in it has", record.Tried)
	}
	if !slices.Equal(steps, []int{rungSeat, rungWait, rungWait}) {
		t.Fatalf("rungs = %v, want the rebuild and then the wait", steps)
	}
	if reason, _ := os.ReadFile(filepath.Join(runDir, "delivery-stop-reason.txt")); len(reason) > 0 {
		t.Fatalf("the delivery ended instead of waiting: %q", reason)
	}
}

// seatTheConsumerConfig gives review-a two candidates with launches, one on
// the vendor review-b holds and one free, in the file the tick reads.
func seatTheConsumerConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"max_stages":3,"design_max_rounds":3,
	 "agents":{"applier":{"command":"true","timeout_seconds":60},
	  "reviewer_agents":[{"reviewer_id":"review-a","agent":{"id":"judge-a"},
	    "candidates":[{"id":"judge-a-held"},{"id":"judge-a-free"}]},
	   {"reviewer_id":"review-b","agent":{"id":"judge-b"}}]},
	 "models":{"reviewers":[
	  {"id":"review-a","vendor":"Vendor A","model":"model-a",
	   "candidates":[{"vendor":"Vendor B","model":"model-b-held"},{"vendor":"Vendor C","model":"model-c"}]},
	  {"id":"review-b","vendor":"Vendor B","model":"model-b"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRoundFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
