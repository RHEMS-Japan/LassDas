package attendant

import (
	"os"
	"path/filepath"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A merge that deployed nothing waits for an operator, exactly as the other
// staging outcomes an operator has to look at do. Left out of that set it
// would show as a plain failure and the delivery would end unattended.
func TestADeployThatNeverStartedWaitsForAnOperator(t *testing.T) {
	if !attentionVerdict("deploy_absent") {
		t.Fatal("deploy_absent does not wait for an operator")
	}
	var status RunStatus
	placeStagingOutcome(&status, "deploy_absent", "")
	if status.Step != "attention" || status.Stage != "staging" {
		t.Fatalf("step = %q stage = %q, want attention at staging", status.Step, status.Stage)
	}
	if status.StepTitle == "" || status.StepTitle == "ステージング反映で停止" {
		t.Fatalf("step title = %q: the generic failure title says the wrong thing", status.StepTitle)
	}
}

// A merge that deployed nothing must never open the promotion. The gate
// reads only the verdict, so widening it by one word would let a delivery
// nothing verified reach production — and the whole suite stayed green
// when that was tried (review of #134).
func TestADeployThatNeverStartedCannotOpenThePromotion(t *testing.T) {
	for _, verdict := range []string{"deploy_absent", "deploy_failed", "merge_unverified", "observe_blocked", "observe_failed", "checks_failed"} {
		if promotableStagingVerdict(verdict) {
			t.Errorf("verdict %q opens the promotion", verdict)
		}
	}
	if !promotableStagingVerdict("pass") {
		t.Error("a passing staging report no longer opens the promotion")
	}
}

// A delivery that stops at the pull request is not finished: a person has to
// merge it, and nothing reaches the repository until they do. Calling it
// "納品済み" moved the card out of the running list and collapsed it to two
// words, so a requester read "delivered" over a pull request that was still
// open and had no reason to look further (live 2026-09-18).
func TestAPullRequestNobodyMergedIsNotDelivered(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	run := state.RunOverview{DeliveryID: "delivery_abc", TerminalCode: string(hook.TerminalSuccess)}
	dir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature-pr.json"), []byte(`{"binding":{"repository":"owner/name"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var status RunStatus
	classifyAfterTerminal(&status, config, run, nil)
	if status.Step == "done" {
		t.Fatalf("an unmerged pull request was reported as finished: %q %q", status.Step, status.StepTitle)
	}
	if status.Step != "confirm" {
		t.Fatalf("step = %q, want the stage a person acts on", status.Step)
	}
	if status.NextAction == "" {
		t.Fatal("the requester is not told what to do next")
	}

	// With no pull request there is nothing for a person to merge.
	if err := os.Remove(filepath.Join(dir, "feature-pr.json")); err != nil {
		t.Fatal(err)
	}
	status = RunStatus{}
	classifyAfterTerminal(&status, config, run, nil)
	if status.Step != "done" {
		t.Fatalf("step = %q, want done when no pull request was opened", status.Step)
	}
}

// The rail a reader sees is the one this installation can reach. It used to
// be a fixed nine on the board, so a destination that stops at the pull
// request drew STG, 確認 and 本番 for every delivery and never lit them: three
// grey stages after the last one that moved, with no way to tell "not yet"
// from "never" (observed 2026-09-18).
func TestTheRailIsTheOneThisInstallationCanReach(t *testing.T) {
	ids := func(stages []BoardStage) []string {
		out := make([]string, 0, len(stages))
		for _, stage := range stages {
			out = append(out, stage.ID)
		}
		return out
	}
	same := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	stopsAtThePullRequest := ids(railStages(runtime.Config{}))
	want := []string{"intake", "investigate", "design", "implement", "review", "checks", "confirm"}
	if !same(stopsAtThePullRequest, want) {
		t.Fatalf("rail = %v, want %v", stopsAtThePullRequest, want)
	}
	for _, stage := range railStages(runtime.Config{}) {
		if stage.Label == "" {
			t.Fatalf("stage %q has nothing to show a reader", stage.ID)
		}
	}
}

// Once the delivered pull request is merged, the delivery is finished. It
// used to rest at マージ待ち for ever: the condition was a successful run, a
// pull request on disk and no post-merge pipeline, none of which change when
// someone merges, so the board told a requester to merge what they had
// merged an hour before and the card never left the running list (live
// 2026-09-18, five of them at once).
func TestAMergedPullRequestFinishesTheDelivery(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	run := state.RunOverview{DeliveryID: "delivery_abc", TerminalCode: string(hook.TerminalSuccess)}
	dir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature-pr.json"), []byte(`{"binding":{"repository":"owner/name"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var waiting RunStatus
	classifyAfterTerminal(&waiting, config, run, nil)
	if waiting.Step != "confirm" {
		t.Fatalf("before the merge: step = %q, want confirm", waiting.Step)
	}

	if err := os.WriteFile(filepath.Join(dir, featureMergeFile),
		[]byte(`{"merged":true,"state":"closed","merge_commit_sha":"abcdef1234567890"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var merged RunStatus
	classifyAfterTerminal(&merged, config, run, nil)
	if merged.Step != "done" {
		t.Fatalf("after the merge: step = %q %q, want done", merged.Step, merged.StepTitle)
	}
	if merged.StepTitle == "マージ待ち" {
		t.Fatal("a merged delivery is still asking to be merged")
	}

	// A record that says it is not merged is not a merge.
	if err := os.WriteFile(filepath.Join(dir, featureMergeFile), []byte(`{"merged":false,"state":"open"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var open RunStatus
	classifyAfterTerminal(&open, config, run, nil)
	if open.Step != "confirm" {
		t.Fatalf("an open pull request was taken for merged: %q", open.Step)
	}
}
