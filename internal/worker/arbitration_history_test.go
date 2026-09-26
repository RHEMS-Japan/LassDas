package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeArbitrationRound(t *testing.T, dir string, number int, config Config, request TicketRequest, source SourceSnapshot, rationale string) (Candidate, []Review) {
	t.Helper()
	candidate, err := NewCandidate(number, ModelCandidateOutput{
		Files: []ModelCandidateFile{{Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n"}}, Rationale: rationale,
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	var reviews []Review
	for i, seat := range config.Models.Reviewers {
		answer := ModelReviewOutput{Verdict: "pass", Findings: []ModelFinding{}}
		if i == 1 {
			answer = ModelReviewOutput{Verdict: "revise", Findings: []ModelFinding{{Code: "missing-check", Path: request.TargetFiles[0], Message: fmt.Sprintf("Finding from round %d", number)}}}
			if number == 1 {
				answer.Findings[0].Code = "past-only"
			}
		}
		review, err := NewReview(number, seat, answer, candidate, source, request, config, validTestInvocation(seat), testInvocationTime)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, review)
	}
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	stageDir := filepath.Join(dir, fmt.Sprintf("stage-%d", number))
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"ticket.json": request, "source.json": source, "candidate.json": candidate, "decision.json": decision} {
		writeArbitrationJSON(t, filepath.Join(stageDir, name), value)
	}
	for _, review := range reviews {
		writeArbitrationJSON(t, filepath.Join(stageDir, review.ReviewerID+".json"), review)
	}
	return candidate, reviews
}

func writeArbitrationJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArbitrationHistoryReachesTheModelAndIsSealedWithTheRuling(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	dir := t.TempDir()
	first, reviews := writeArbitrationRound(t, dir, 1, config, request, source, "Tried adding registration; it exceeded the requested scope.")
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(instructingAnswer)})
	prior, err := invoker.Arbitrate(t.Context(), first, reviews, nil, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	writeArbitrationJSON(t, filepath.Join(dir, "stage-1", RulingFileName), prior)
	failure := NewValidationFailure(1, "run-validation", "the expected behavior is missing")
	failure.DeliveryID, failure.InputSHA256, failure.ConfigSHA256, failure.ToolSHA = request.DeliveryID, request.InputSHA256, request.ConfigSHA256, request.ToolSHA
	if err := failure.Seal(); err != nil {
		t.Fatal(err)
	}
	writeArbitrationJSON(t, filepath.Join(dir, "stage-1", "validation-failure.json"), failure)
	writeArbitrationRound(t, dir, 2, config, request, source, "Removed registration; the original finding returned.")
	current, currentReviews := writeArbitrationRound(t, dir, 3, config, request, source, "Current attempt.")
	history, err := LoadArbitrationHistory(dir, 3, request, config)
	if err != nil || len(history.Rounds) != 2 || len(history.Unavailable) != 0 {
		t.Fatalf("history=%+v, error=%v", history, err)
	}
	if history.Rounds[0].Ruling == nil || history.Rounds[0].Ruling.SHA256 != prior.RulingSHA256 || history.Rounds[0].Validation == nil {
		t.Fatal("the earlier ruling or validation refusal was lost")
	}
	api := &fakeChatAPI{output: chatOutput(instructingAnswer)}
	invoker, _ = NewModelInvoker(api)
	ruling, err := invoker.Arbitrate(t.Context(), current, currentReviews, nil, nil, source, request, config, testInvocationTime, history)
	if err != nil {
		t.Fatal(err)
	}
	var prompt struct {
		Previous *ArbitrationHistory `json:"previous_attempts"`
	}
	if err := json.Unmarshal([]byte(api.request.Messages[1].Content), &prompt); err != nil || !reflect.DeepEqual(prompt.Previous, history) {
		t.Fatalf("the model did not receive the validated history: %v", err)
	}
	if !reflect.DeepEqual(ruling.History, history) {
		t.Fatal("the sealed ruling did not record what history the model saw")
	}
	for _, phrase := range []string{"do not prescribe the same failed approach", "explicit scope and prohibitions", "only standing_findings in the current round", "unavailable_rounds"} {
		if !strings.Contains(api.request.Messages[0].Content, phrase) {
			t.Errorf("arbitration instruction lost %q", phrase)
		}
	}
	encoded, _ := json.Marshal(ruling)
	var edited Ruling
	if err := json.Unmarshal(encoded, &edited); err != nil {
		t.Fatal(err)
	}
	edited.History.Rounds[0].Rationale = "A different account."
	if edited.Validate(current, currentReviews, request, config) == nil {
		t.Fatal("edited history kept the ruling's seal valid")
	}
	// Historical findings cannot be counted as current objections.
	api.output = chatOutput(strings.Replace(overrulingAnswer(config.Models.Reviewers[1].ID, request.TargetFiles[0]), "missed-escalation", "past-only", 1))
	if _, err := invoker.Arbitrate(t.Context(), current, currentReviews, nil, nil, source, request, config, testInvocationTime, history); err == nil {
		t.Fatal("a finding absent from the current round was overruled")
	}
}

func TestArbitrationHistoryDoesNotTurnBadEarlierRecordsIntoAnotherFailure(t *testing.T) {
	for _, record := range []string{"ticket.json", "source.json", "candidate.json", "review-b.json", "decision.json"} {
		t.Run(record, func(t *testing.T) {
			config, request, source := validArtifactFixture(t)
			dir := t.TempDir()
			writeArbitrationRound(t, dir, 1, config, request, source, "Unusable earlier attempt.")
			current, reviews := writeArbitrationRound(t, dir, 2, config, request, source, "Sound current round.")
			path := filepath.Join(dir, "stage-1", record)
			if err := os.WriteFile(path, []byte(`{"unknown_record_field":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			history, err := LoadArbitrationHistory(dir, 2, request, config)
			if err != nil || len(history.Rounds) != 0 || !reflect.DeepEqual(history.Unavailable, []int{1}) {
				t.Fatalf("bad history was hidden or stopped the ruling: %+v %v", history, err)
			}
			invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(instructingAnswer)})
			if _, err := invoker.Arbitrate(t.Context(), current, reviews, nil, nil, source, request, config, testInvocationTime, history); err != nil {
				t.Fatal(err)
			}
			kept, err := os.ReadFile(path)
			if err != nil || string(kept) != `{"unknown_record_field":true}` {
				t.Fatal("history reading changed the source record")
			}
		})
	}
	for _, change := range []string{"delivery", "input", "request"} {
		t.Run("foreign "+change, func(t *testing.T) {
			config, request, source := validArtifactFixture(t)
			dir := t.TempDir()
			writeArbitrationRound(t, dir, 1, config, request, source, "Different request.")
			switch change {
			case "delivery":
				request.DeliveryID = "delivery_" + strings.Repeat("cd", 16)
			case "input":
				request.InputSHA256 = strings.Repeat("cd", 32)
			case "request":
				request.Request = "A different request."
			}
			history, err := LoadArbitrationHistory(dir, 2, request, config)
			if err != nil || len(history.Rounds) != 0 || !reflect.DeepEqual(history.Unavailable, []int{1}) {
				t.Fatalf("foreign history admitted: %+v %v", history, err)
			}
		})
	}
}

func TestArbitrationHistoryBoundsContextAndNamesEveryOmittedRound(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	dir := t.TempDir()
	for number := 1; number < StageCeiling; number++ {
		writeArbitrationRound(t, dir, number, config, request, source, strings.TrimSpace(strings.Repeat("An attempted fix. ", 90)))
	}
	history, err := LoadArbitrationHistory(dir, StageCeiling, request, config)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(history)
	if len(encoded) > maxArbitrationHistoryBytes || len(history.Omitted) == 0 || len(history.Unavailable) != 0 || len(history.Rounds) == 0 {
		t.Fatalf("history is not bounded or lost valid rounds: %d bytes, %+v", len(encoded), history)
	}
	if history.Rounds[len(history.Rounds)-1].Round != StageCeiling-1 || len(history.Rounds)+len(history.Omitted) != StageCeiling-1 {
		t.Fatal("recent history or omission accounting was lost")
	}
	history.Omitted = history.Omitted[1:]
	if history.validate(StageCeiling, request, config) == nil {
		t.Fatal("a round was silently dropped")
	}
}

func TestArbitrationHistoryIsOptionalForExistingRulings(t *testing.T) {
	config, request, source, candidate, reviews := deadlockedRoundAt(t, 1)
	history, err := LoadArbitrationHistory(t.TempDir(), 1, request, config)
	if err != nil || history != nil {
		t.Fatalf("first round history=%+v %v", history, err)
	}
	invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(instructingAnswer)})
	ruling, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, nil, source, request, config, testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(ruling)
	if strings.Contains(string(encoded), "previous_attempts") || ruling.Validate(candidate, reviews, request, config) != nil {
		t.Fatal("an old ruling without history changed shape or stopped validating")
	}
}

func TestArbitrationHistoryExcludesUnboundOptionalEvidence(t *testing.T) {
	for _, kind := range []string{"ruling delivery", "ruling candidate", "ruling edited", "validation delivery", "validation input", "validation config", "validation tool", "validation round", "validation edited"} {
		t.Run(kind, func(t *testing.T) {
			config, request, source := validArtifactFixture(t)
			dir := t.TempDir()
			candidate, reviews := writeArbitrationRound(t, dir, 1, config, request, source, "An earlier rejected attempt.")
			isRuling := strings.HasPrefix(kind, "ruling")
			if isRuling {
				invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(instructingAnswer)})
				ruling, err := invoker.Arbitrate(t.Context(), candidate, reviews, nil, nil, source, request, config, testInvocationTime)
				if err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "ruling delivery":
					ruling.DeliveryID = "delivery_" + strings.Repeat("cd", 16)
				case "ruling candidate":
					ruling.CandidateSHA256 = strings.Repeat("cd", 32)
				}
				ruling, err = sealRuling(ruling)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "ruling edited" {
					ruling.Instruction = "Changed without sealing."
				}
				writeArbitrationJSON(t, filepath.Join(dir, "stage-1", RulingFileName), ruling)
			} else {
				failure := NewValidationFailure(1, "run-validation", "The request's check failed.")
				failure.DeliveryID, failure.InputSHA256, failure.ConfigSHA256, failure.ToolSHA = request.DeliveryID, request.InputSHA256, request.ConfigSHA256, request.ToolSHA
				switch kind {
				case "validation delivery":
					failure.DeliveryID = "delivery_" + strings.Repeat("cd", 16)
				case "validation input":
					failure.InputSHA256 = strings.Repeat("cd", 32)
				case "validation config":
					failure.ConfigSHA256 = strings.Repeat("cd", 32)
				case "validation tool":
					failure.ToolSHA = strings.Repeat("cd", 20)
				case "validation round":
					failure.Round = 2
				}
				if err := failure.Seal(); err != nil {
					t.Fatal(err)
				}
				if kind == "validation edited" {
					failure.Output = "Changed without sealing."
				}
				writeArbitrationJSON(t, filepath.Join(dir, "stage-1", "validation-failure.json"), failure)
			}
			history, err := LoadArbitrationHistory(dir, 2, request, config)
			if err != nil || history == nil || len(history.Rounds) != 1 || len(history.Unavailable) != 0 {
				t.Fatalf("optional bad evidence discarded a sound earlier round: %+v %v", history, err)
			}
			round := history.Rounds[0]
			label := "validation"
			if isRuling {
				label = "ruling"
			}
			if round.Ruling != nil || round.Validation != nil || !reflect.DeepEqual(round.Unavailable, []string{label}) {
				t.Fatalf("unbound evidence was not explicitly excluded: %+v", round)
			}
		})
	}
}

func TestArbitrationHistoryMissingOrMisnumberedRoundIsNotEvidence(t *testing.T) {
	for _, shifted := range []bool{false, true} {
		t.Run(fmt.Sprint(shifted), func(t *testing.T) {
			config, request, source := validArtifactFixture(t)
			dir := t.TempDir()
			if shifted {
				writeArbitrationRound(t, dir, 2, config, request, source, "A later attempt in the wrong directory.")
				if err := os.Rename(filepath.Join(dir, "stage-2"), filepath.Join(dir, "stage-1")); err != nil {
					t.Fatal(err)
				}
			}
			history, err := LoadArbitrationHistory(dir, 2, request, config)
			if err != nil || history == nil || len(history.Rounds) != 0 || !reflect.DeepEqual(history.Unavailable, []int{1}) {
				t.Fatalf("absent or shifted history was admitted: %+v %v", history, err)
			}
		})
	}
}

func TestArbitrationHistoryUsesEachAttemptsOwnFiles(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	dir := t.TempDir()
	writeArbitrationRound(t, dir, 1, config, request, source, "This attempt changed a file absent from the next one.")
	priorFile := request.TargetFiles[0]
	request.TargetFiles = []string{"client/src/components/Another.tsx"}
	history, err := LoadArbitrationHistory(dir, 2, request, config)
	if err != nil || history == nil || len(history.Rounds) != 1 || history.Rounds[0].Files[0] != priorFile {
		t.Fatalf("changed file selection made an earlier attempt unreadable: %+v %v", history, err)
	}
}
