package main

import (
	"fmt"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

func promptFixtureFiles(fileCount, lineCount int) (worker.Candidate, worker.SourceSnapshot) {
	var candidate worker.Candidate
	var source worker.SourceSnapshot
	for f := 0; f < fileCount; f++ {
		before := make([]string, 0, lineCount)
		after := make([]string, 0, lineCount)
		for l := 0; l < lineCount; l++ {
			line := fmt.Sprintf("file-%d line-%d original-payload", f, l)
			before = append(before, line)
			if l%2 == 1 {
				line = fmt.Sprintf("file-%d line-%d changed-payload", f, l)
			}
			after = append(after, line)
		}
		path := fmt.Sprintf("client/src/file-%d.ts", f)
		source.Files = append(source.Files, worker.SourceFile{Path: path, Content: strings.Join(before, "\n")})
		candidate.Files = append(candidate.Files, worker.CandidateFile{Path: path, Content: strings.Join(after, "\n")})
	}
	return candidate, source
}

func promptFixtureRequest() worker.TicketRequest {
	return worker.TicketRequest{IssueKey: "TEST-1", Summary: "件名", Request: "本文"}
}

// A full slate of maximum-size objections would eat the whole instruction
// budget on its own; the tail is dropped and the instruction says so.
func TestReviewAgentPromptBoundsPreviousFindings(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	long := strings.Repeat("あ", 1300)
	findings := make([]worker.ModelFinding, 0, 128)
	for index := 0; index < 128; index++ {
		findings = append(findings, worker.ModelFinding{Code: "bulk-objection", Path: "client/src/label.ts", Message: long})
	}
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, findings, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > worker.MaxAgentPromptBytes {
		t.Fatalf("prompt bytes = %d", len(prompt))
	}
	if !strings.Contains(prompt, "省略") || !strings.Contains(prompt, "bulk-objection") {
		t.Fatal("the bounded findings did not say what was kept and dropped")
	}
}

func TestReviewAgentPromptCarriesPreviousFindings(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	findings := []worker.ModelFinding{{Code: "stale-caller", Path: "client/src/label.ts", Message: "A caller still expects the old text."}}
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, findings, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "前の巡で出た指摘") || !strings.Contains(prompt, "stale-caller") {
		t.Fatal("the earlier objections were not carried to the judge")
	}
}

// A small change travels with its full patches, exactly as before.
func TestReviewAgentPromptEmbedsThePatchesWhenTheyFit(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "機械抽出の差分") || !strings.Contains(prompt, "changed-payload") {
		t.Fatal("a small change lost its embedded patch")
	}
}

// When the full patches outgrow the instruction budget, the reviewer still
// gets an instruction: the changed line ranges without their content. The
// first oversized live candidate (15 files, ~550KB of change) died as
// "instruction is too large" with no review at all; this pins the fallback.
func TestReviewAgentPromptFallsBackToOutlinesWhenPatchesOvergrow(t *testing.T) {
	candidate, source := promptFixtureFiles(40, 120)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > worker.MaxAgentPromptBytes {
		t.Fatalf("the fallback still exceeds the budget: %d bytes", len(prompt))
	}
	if !strings.Contains(prompt, "変更位置の一覧") {
		t.Fatal("the oversized change did not switch to the outline form")
	}
	if !strings.Contains(prompt, "@@ 変更前") {
		t.Fatal("the outline lost the changed line ranges")
	}
	if strings.Contains(prompt, "changed-payload") {
		t.Fatal("the outline still carries patch content")
	}
	if !strings.Contains(prompt, "client/src/file-39.ts") {
		t.Fatal("the outline dropped a changed file")
	}
}

// The candidate reviewer is shown a label of the shape it must write, and
// told that the signal sending a delivery back to its design is written
// alone. The label is normalised on the way in, and normalisation refuses
// to manufacture that signal, so a reviewer writing "design_wrong" loses
// it: this line is what keeps that from happening.
func TestTheCandidateReviewerIsToldHowToWriteTheSignal(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatalf("reviewAgentPrompt: %v", err)
	}
	for _, want := range []string{
		`"code":"missing-null-check"`,
		"code は英小文字と数字とハイフンだけの短い識別子です",
		"code をちょうど design-wrong とし",
		"design-wrong に語を足さないでください",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the reviewer's instruction lacks %q", want)
		}
	}
}

// A reviewer is not the one that runs the build. A live review returned
// revise for the sole reason that its sandbox had no compiler, so it had not
// seen the build, the vet pass or the tests succeed - and the round repeated
// until the ceiling ended the run, because the next reviewer had no compiler
// either. The commands run in the validation stage, after the verdict.
func TestTheCandidateReviewerIsNotAskedToRunTheBuild(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatalf("reviewAgentPrompt: %v", err)
	}
	for _, want := range []string{
		"評決の対象は差分そのものです",
		"ビルド・vet・テストの成否は、この評決のあとの検証段が、隔離した環境で実際にコマンドを実行して確かめます",
		"実行結果を見ていないことを理由に revise にしないでください",
		"この実行環境にコマンドが無くても同じです",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the reviewer's instruction lacks %q", want)
		}
	}
}

// What a delivery's description will say, and what the environment will look
// like once the change ships, are not in front of the reviewer. A live review
// returned revise because the request wanted a configuration example and a
// check command in the pull request description: text that does not exist
// while the code is judged, and that no round of implementation could add.
func TestTheCandidateReviewerIsToldWhatLiesOutsideTheDiff(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatalf("reviewAgentPrompt: %v", err)
	}
	for _, want := range []string{
		"PR の説明文",
		"納品後の環境の状態",
		"は評決の対象外です",
		"差分を読んで判断できる部分だけを見てください",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the reviewer's instruction lacks %q", want)
		}
	}
}

// The instruction told the reviewer twice to turn anything it could not
// confirm into a revise. Read literally that covers everything outside the
// diff, which is how both looping runs began. Both sentences now hold only
// for what reading the diff was supposed to settle.
func TestTheCandidateReviewerRevisesOnlyForWhatTheDiffCanShow(t *testing.T) {
	candidate, source := promptFixtureFiles(1, 40)
	prompt, err := reviewAgentPrompt(candidate, source, promptFixtureRequest(),
		worker.ModelEndpoint{Lens: "correctness"}, nil, nil, "", "/tmp/review repo", false)
	if err != nil {
		t.Fatalf("reviewAgentPrompt: %v", err)
	}
	for _, gone := range []string{
		"判断に迷ったら、未確認の点と確かめられなかった理由を findings の message に書いて revise にしてください",
		"確信が持てない点が残ったら、未確認の点と確かめられなかった理由を findings の message に書いて revise としてください",
	} {
		if strings.Contains(prompt, gone) {
			t.Errorf("the unconditional unconfirmed-means-revise rule survives: %q", gone)
		}
	}
	for _, want := range []string{
		"差分を読めば確かめられるはずのことが確かめられなかったときだけ",
		"差分を読めば確かめられるはずのことが確かめられないまま残ったときだけ",
		"対象外のものを未確認として revise にしないでください",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the reviewer's instruction lacks %q", want)
		}
	}
}
