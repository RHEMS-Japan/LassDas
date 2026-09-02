package worker

import (
	"os"
	"path/filepath"
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

	// Reverting the submitted file to its base content leaves no tracked
	// change at all — the content check is what still catches it.
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Send';\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that reverted the change was accepted")
	}

	writeAgentFile(t, root, "client/src/label.ts", submitted)
	writeAgentFile(t, root, "README.md", "fixture rewritten by the reviewer\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that edited a tracked file outside the candidate was accepted")
	}
}

// A reviewer that runs the repository's own tests leaves untracked and
// ignored byproducts behind. The published change is built from the sealed
// candidate, so those files are noise, not tampering — treating them as
// tampering killed RFDEV-674 after its review had passed.
func TestConfirmTreeMatchesCandidateToleratesReviewerToolingByproducts(t *testing.T) {
	root, _ := buildAgentRepository(t)
	consumer := fixtureConsumerForAgent()
	submitted := "export const submitLabel = 'Submit';\n"
	writeAgentFile(t, root, "client/src/label.ts", submitted)
	candidate := Candidate{Files: []CandidateFile{{Path: "client/src/label.ts", Content: submitted}}}

	writeAgentFile(t, root, "client/src/vitest.config.ts.timestamp-1.mjs", "export default {};\n")
	writeAgentFile(t, root, ".gitignore", "*.tsbuildinfo\n")
	writeAgentFile(t, root, "client/src/tsconfig.tsbuildinfo", "{}\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err != nil {
		t.Fatalf("a reviewer's tooling byproducts were treated as tampering: %v", err)
	}
}

// After a review the tree is the next round's starting point. Everything the
// reviewer left behind must go — untracked and ignored, files and
// directories, hidden or not — while the candidate's own new file and the
// tracked tree stay exactly as they are.
func TestCleanReviewByproductsRemovesOnlyWhatTheReviewerLeft(t *testing.T) {
	root, _ := buildAgentRepository(t)
	submitted := "export const submitLabel = 'Submit';\n"
	added := "export const added = true;\n"
	writeAgentFile(t, root, "client/src/label.ts", submitted)
	writeAgentFile(t, root, "client/src/extra.test.ts", added)
	candidate := Candidate{Files: []CandidateFile{
		{Path: "client/src/extra.test.ts", Content: added},
		{Path: "client/src/label.ts", Content: submitted},
	}}

	writeAgentFile(t, root, ".gitignore", "*.tsbuildinfo\ncoverage/\n")
	writeAgentFile(t, root, "client/src/vitest.config.ts.timestamp-1.mjs", "export default {};\n")
	writeAgentFile(t, root, "client/src/tsconfig.tsbuildinfo", "{}\n")
	writeAgentFile(t, root, "client/coverage/lcov.info", "TN:\n")
	writeAgentFile(t, root, ".eslintcache", "[]\n")
	writeAgentFile(t, root, "scratch/notes/todo.md", "later\n")

	if err := CleanReviewByproducts(root, candidate); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}
	for _, gone := range []string{
		".gitignore", "client/src/vitest.config.ts.timestamp-1.mjs", "client/src/tsconfig.tsbuildinfo",
		"client/coverage", ".eslintcache", "scratch",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Fatalf("%s survived the cleanup (%v)", gone, err)
		}
	}
	for path, expected := range map[string]string{
		"client/src/label.ts": submitted, "client/src/extra.test.ts": added, "README.md": "fixture\n",
	} {
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil || string(actual) != expected {
			t.Fatalf("%s was not left alone: %q %v", path, actual, err)
		}
	}
	if err := ConfirmTreeMatchesCandidate(root, candidate, fixtureConsumerForAgent()); err != nil {
		t.Fatalf("the cleaned tree no longer matches the candidate: %v", err)
	}
}

// A reviewer that writes an ignore rule can make git fold the directory
// holding a submitted new file into a single ignored entry. The cleanup must
// refuse rather than delete the candidate's file along with it.
func TestCleanReviewByproductsRefusesToDeleteADirectoryHoldingTheCandidate(t *testing.T) {
	root, _ := buildAgentRepository(t)
	added := "export const added = true;\n"
	writeAgentFile(t, root, "client/src/newdir/extra.ts", added)
	candidate := Candidate{Files: []CandidateFile{{Path: "client/src/newdir/extra.ts", Content: added}}}
	writeAgentFile(t, root, "client/src/.gitignore", "newdir/\n")

	if err := CleanReviewByproducts(root, candidate); err == nil {
		t.Fatal("a directory holding the candidate's new file was deleted as a byproduct")
	}
	if _, err := os.Stat(filepath.Join(root, "client", "src", "newdir", "extra.ts")); err != nil {
		t.Fatalf("the candidate's new file did not survive: %v", err)
	}
}

// A reviewer that commits makes its edits invisible to a status scan; the
// head recorded before the review is what exposes it.
func TestRepositoryHeadMovesWhenTheReviewerCommits(t *testing.T) {
	root, base := buildAgentRepository(t)
	before, err := RepositoryHead(root)
	if err != nil || before != base {
		t.Fatalf("head was not read: %q %v (base %q)", before, err, base)
	}
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Reviewer committed this';\n")
	agentGit(t, root, "add", "-A")
	agentGit(t, root, "-c", "user.name=reviewer", "-c", "user.email=reviewer@example.invalid", "commit", "-qm", "quiet fix")
	after, err := RepositoryHead(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("a commit did not move the recorded head")
	}
}

// A submitted new file is untracked, so the tracked-change scan cannot see
// it; the content check must still notice when the reviewer rewrites it.
func TestConfirmTreeMatchesCandidateChecksASubmittedNewFile(t *testing.T) {
	root, _ := buildAgentRepository(t)
	consumer := fixtureConsumerForAgent()
	submitted := "export const added = true;\n"
	writeAgentFile(t, root, "client/src/extra.test.ts", submitted)
	candidate := Candidate{Files: []CandidateFile{{Path: "client/src/extra.test.ts", Content: submitted}}}

	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err != nil {
		t.Fatalf("an untouched new file was rejected: %v", err)
	}

	writeAgentFile(t, root, "client/src/extra.test.ts", "export const added = false;\n")
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that rewrote a submitted new file was accepted")
	}

	if err := os.Remove(filepath.Join(root, "client", "src", "extra.test.ts")); err != nil {
		t.Fatal(err)
	}
	if err := ConfirmTreeMatchesCandidate(root, candidate, consumer); err == nil {
		t.Fatal("a reviewer that deleted a submitted new file was accepted")
	}
}
