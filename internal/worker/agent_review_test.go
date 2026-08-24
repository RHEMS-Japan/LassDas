package worker

import (
	"strings"
	"testing"
	"time"
)

func sealedReviewRun(t *testing.T, agent AgentConfig, request TicketRequest, source SourceSnapshot, changed []string) AgentRun {
	t.Helper()
	run, err := SealAgentRun(AgentRun{
		SchemaVersion: ArtifactSchemaVersion, Stage: 1,
		DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, BaseSHA: source.BaseSHA,
		AgentID: agent.ID, Command: agent.Command, PromptBytes: 42,
		ChangedFiles: changed,
		Transcript:   ReviewAnswerRulesTail + "\n" + `{"verdict":"pass","findings":[]}`,
		RanAt:        time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// A run naming any agent other than the reviewer's own launch is not this
// reviewer's judgment, whatever its verdict reads — this is where the
// profile binding is enforced.
func TestAgentReviewFromRunRejectsAForeignLaunch(t *testing.T) {
	config, request, source, candidate := validCandidate(t)
	endpoint := config.Models.Reviewers[1]
	foreign := sealedReviewRun(t, config.Agents.Implementer, request, source, []string{request.TargetFiles[0]})
	if _, err := AgentReviewFromRun(endpoint, foreign, candidate, source, request, config, testInvocationTime); err == nil {
		t.Fatal("AgentReviewFromRun() accepted the implementer's run as a review")
	}
	own := sealedReviewRun(t, config.Agents.Reviewer, request, source, nil)
	if _, err := AgentReviewFromRun(endpoint, own, candidate, source, request, config, testInvocationTime); err != nil {
		t.Fatalf("AgentReviewFromRun() rejected the reviewer's own launch: %v", err)
	}
}

// A CLI agent writes prose and then its answer, so the verdict has to be found
// in ordinary output rather than in a clean response body.
func TestDecodeAgentReviewOutputFindsTheVerdictInOrdinaryOutput(t *testing.T) {
	transcript := strings.Join([]string{
		"Reading client/src/label.ts...",
		"The change replaces the submit label. I looked at the callers too.",
		"",
		"Verdict:",
		`{"verdict":"revise","findings":[{"code":"missing-case","path":"client/src/label.ts","line":12,"message":"The cancel path still shows the old label."}]}`,
	}, "\n")

	output, err := DecodeAgentReviewOutput(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "revise" || len(output.Findings) != 1 || output.Findings[0].Code != "missing-case" {
		t.Fatalf("verdict was not read: %+v", output)
	}
}

func TestDecodeAgentReviewOutputIgnoresBracesInProse(t *testing.T) {
	transcript := strings.Join([]string{
		"The function body `{ return submitLabel }` is unchanged, and the object",
		"literal {a: 1} is untouched.",
		`{"verdict":"pass","findings":[]}`,
		"Done.",
	}, "\n")

	output, err := DecodeAgentReviewOutput(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "pass" || len(output.Findings) != 0 {
		t.Fatalf("verdict was not read: %+v", output)
	}
}

// A findings array holds nested objects, so the scan has to balance braces
// rather than take the last one it sees.
func TestDecodeAgentReviewOutputReadsANestedVerdict(t *testing.T) {
	transcript := `Here is my answer.
{"verdict":"revise","findings":[{"code":"a","path":"client/src/label.ts","message":"first"},{"code":"b","path":"client/src/label.ts","message":"second"}]}`

	output, err := DecodeAgentReviewOutput(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Findings) != 2 || output.Findings[1].Code != "b" {
		t.Fatalf("nested verdict was not read: %+v", output)
	}
}

// A brace inside a quoted message must not end the object early.
func TestDecodeAgentReviewOutputHandlesBracesInsideMessages(t *testing.T) {
	transcript := `{"verdict":"revise","findings":[{"code":"a","path":"client/src/label.ts","message":"the block { } is empty"}]}`

	output, err := DecodeAgentReviewOutput(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Findings) != 1 || !strings.Contains(output.Findings[0].Message, "{ }") {
		t.Fatalf("quoted braces broke the scan: %+v", output)
	}
}

// When the agent talks about a verdict without printing one, that is a failed
// review rather than a guess in either direction.
func TestDecodeAgentReviewOutputRefusesToGuess(t *testing.T) {
	for _, transcript := range []string{
		"I think this passes.",
		"",
		`{"summary":"looks fine"}`,
		"verdict: pass",
		`{"verdict":`,
	} {
		if _, err := DecodeAgentReviewOutput(transcript); err == nil {
			t.Fatalf("a verdict was invented from %q", transcript)
		}
	}
}

// A reviewing agent's transcript usually echoes its instruction, and the
// instruction contains verdict-shaped format examples. When the agent answers
// with prose and no verdict, the echoed examples must not be mistaken for its
// answer: that turns "the agent never judged" into a misleading validation
// failure. Observed live: a reviewer asked for permission to start instead of
// reviewing, and the decoder picked the instruction's revise example.
func TestDecodeAgentReviewOutputIgnoresEchoedFormatExamples(t *testing.T) {
	echoedInstruction := strings.Join([]string{
		"## 答え方 (最後にこの形の JSON だけを出力する)",
		`{"verdict":"pass","findings":[]}`,
		"または",
		`{"verdict":"revise","findings":[{"code":"英小文字とハイフンの短い識別子","path":"変更されたファイルのいずれか","line":0,"message":"何がどう問題かを一文で"}]}`,
		"- verdict が pass のときは findings を空にしてください。revise のときは 1 件以上必要です。",
		ReviewAnswerRulesTail,
	}, "\n")

	withoutAnswer := echoedInstruction + "\n■ 体制: バディ\n受入条件への着手承認をお願いします。"
	if _, err := DecodeAgentReviewOutput(withoutAnswer); err == nil {
		t.Fatal("an echoed format example was taken as the verdict")
	}

	withAnswer := echoedInstruction + "\nRead the files.\n" + `{"verdict":"pass","findings":[]}`
	output, err := DecodeAgentReviewOutput(withAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "pass" {
		t.Fatalf("the real verdict after the echo was not read: %+v", output)
	}
}

// An earlier draft verdict must not win over the final one.
func TestDecodeAgentReviewOutputTakesTheLastVerdict(t *testing.T) {
	transcript := strings.Join([]string{
		`My first impression was {"verdict":"pass","findings":[]}`,
		"but then I read the callers.",
		`{"verdict":"revise","findings":[{"code":"caller","path":"client/src/label.ts","message":"A caller still expects the old text."}]}`,
	}, "\n")

	output, err := DecodeAgentReviewOutput(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if output.Verdict != "revise" {
		t.Fatalf("an earlier draft verdict was taken: %+v", output)
	}
}

func TestConfirmTreeMatchesCandidateRejectsAReviewerThatEdits(t *testing.T) {
	root, _ := buildAgentRepository(t)
	consumer := fixtureConsumerForAgent()
	submitted := "export const submitLabel = 'Submit';\n"
	writeAgentFile(t, root, "client/src/label.ts", submitted)
	candidate := Candidate{Files: []CandidateFile{{Path: "client/src/label.ts", Content: submitted}}}

	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err != nil {
		t.Fatalf("an untouched tree was rejected: %v", err)
	}

	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Reviewer edited this';\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that rewrote the change was accepted")
	}

	writeAgentFile(t, root, "client/src/label.ts", submitted)
	writeAgentFile(t, root, "client/src/extra.ts", "export const added = true;\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that added a file was accepted")
	}
}
