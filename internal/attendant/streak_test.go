package attendant

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

func TestDetectFailureStreakCountsOnlyTheNewestRunOfIdenticalFailures(t *testing.T) {
	never := func(state.RunOverview) bool { return false }
	run := func(id string, claimed int64, st, code string) state.RunOverview {
		return state.RunOverview{RunID: id, DeliveryID: id, State: st, ClaimedAt: claimed, TerminalCode: code}
	}
	cases := map[string]struct {
		runs       []state.RunOverview
		limit      int
		resolved   func(state.RunOverview) bool
		wantCount  int
		wantActive bool
		wantNewest string
	}{
		"three identical failures": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "model_failed"), run("c", 3, "terminal", "model_failed")},
			limit: 3, resolved: never, wantCount: 3, wantActive: true, wantNewest: "c",
		},
		"a success in between": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "success"), run("c", 3, "terminal", "model_failed"), run("d", 4, "terminal", "model_failed")},
			limit: 3, resolved: never, wantCount: 2, wantActive: false, wantNewest: "d",
		},
		"a different failure resets the count": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "validation_failed"), run("c", 3, "terminal", "model_failed")},
			limit: 3, resolved: never, wantCount: 1, wantActive: false, wantNewest: "c",
		},
		"an in-flight run is not an ending": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "model_failed"), run("c", 3, "claimed", ""), run("d", 4, "terminal", "model_failed")},
			limit: 3, resolved: never, wantCount: 3, wantActive: true, wantNewest: "d",
		},
		"a refused ticket is not a failure of ours": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "input_rejected"), run("c", 3, "terminal", "model_failed")},
			limit: 3, resolved: never, wantCount: 1, wantActive: false, wantNewest: "c",
		},
		// An implementer that reports instead of changing is answering one
		// ticket honestly; three in a row is the implementer or its
		// instruction, and the hold has to trip on it like any other
		// repeated ending.
		"three returned implementations hold like any failure": {
			runs: []state.RunOverview{
				run("a", 1, "terminal", "implementation_returned"),
				run("b", 2, "terminal", "implementation_returned"),
				run("c", 3, "terminal", "implementation_returned"),
			},
			limit: 3, resolved: never, wantCount: 3, wantActive: true, wantNewest: "c",
		},
		"an operator's resolution ends the walk": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "model_failed"), run("c", 3, "terminal", "model_failed")},
			limit: 3, resolved: func(r state.RunOverview) bool { return r.RunID == "b" }, wantCount: 1, wantActive: false, wantNewest: "c",
		},
		"limit 0 never holds": {
			runs:  []state.RunOverview{run("a", 1, "terminal", "model_failed"), run("b", 2, "terminal", "model_failed"), run("c", 3, "terminal", "model_failed")},
			limit: 0, resolved: never, wantCount: 3, wantActive: false, wantNewest: "c",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			got := detectFailureStreak(testCase.runs, testCase.limit, testCase.resolved)
			if got.Count != testCase.wantCount || got.Active != testCase.wantActive || got.Newest.RunID != testCase.wantNewest {
				t.Fatalf("detectFailureStreak() = count %d active %v newest %q, want %d %v %q", got.Count, got.Active, got.Newest.RunID, testCase.wantCount, testCase.wantActive, testCase.wantNewest)
			}
		})
	}
}

func TestHoldForStreakPostsOnceAndLiftsOnConfirmation(t *testing.T) {
	root := t.TempDir()
	config := runtime.Config{}
	config.Chain.RunsRoot = root
	runDir := filepath.Join(root, "d9")
	tracker := runtime.TrackerConfig{AllowedCreatorID: 7001}
	newest := state.RunOverview{RunID: "TKT-9", DeliveryID: "d9", IssueID: 90, IssueKey: "TKT-9", State: "terminal", TerminalCode: "model_failed", ClaimedAt: 3}
	streak := failureStreak{Code: "model_failed", Count: 3, Newest: newest, Active: true}
	source := &fakeConfirmationSource{}
	logger := resolutionTestLogger{}

	if !holdForStreak(context.Background(), tracker, source, streak, runDir, logger) {
		t.Fatal("the hold must stay active until an operator confirms")
	}
	if len(source.added) != 1 || hook.ExtractCommentMarker(source.added[0]) != hook.CommentMarker(string(hook.RunCommentStreakHold), "TKT-9") {
		t.Fatalf("notices = %d", len(source.added))
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 10, UserID: 1, Body: source.added[0]})
	// Within the check interval the ticket is not read again.
	if !holdForStreak(context.Background(), tracker, source, streak, runDir, logger) || source.listings != 1 {
		t.Fatalf("the held ticket must not be read every tick; listings = %d", source.listings)
	}
	if err := os.Remove(filepath.Join(runDir, streakCheckFile)); err != nil {
		t.Fatal(err)
	}
	if !holdForStreak(context.Background(), tracker, source, streak, runDir, logger) || len(source.added) != 1 {
		t.Fatalf("the notice must be posted once; notices = %d", len(source.added))
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 11, UserID: 7009, Body: "確認済み"})
	_ = os.Remove(filepath.Join(runDir, streakCheckFile))
	if !holdForStreak(context.Background(), tracker, source, streak, runDir, logger) {
		t.Fatal("a stranger's word must not lift the hold")
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 12, UserID: 7001, Body: "確認済み"})
	_ = os.Remove(filepath.Join(runDir, streakCheckFile))
	if holdForStreak(context.Background(), tracker, source, streak, runDir, logger) {
		t.Fatal("the requester's word after the notice must lift the hold")
	}
	if len(source.added) != 2 || hook.ExtractCommentMarker(source.added[1]) != hook.CommentMarker(string(hook.RunCommentStreakResolved), "TKT-9") {
		t.Fatalf("acknowledgements = %d", len(source.added))
	}
	if _, err := os.Stat(filepath.Join(runDir, streakResolutionFile)); err != nil {
		t.Fatal("the resolution must be recorded in the newest run's directory")
	}
	older := []state.RunOverview{
		{RunID: "TKT-7", DeliveryID: "d7", State: "terminal", TerminalCode: "model_failed", ClaimedAt: 1},
		{RunID: "TKT-8", DeliveryID: "d8", State: "terminal", TerminalCode: "model_failed", ClaimedAt: 2},
		newest,
	}
	if again := detectFailureStreak(older, 3, streakResolvedIn(config)); again.Active || again.Count != 0 {
		t.Fatalf("the detection must stop at the resolved run, got count %d active %v", again.Count, again.Active)
	}
	if notice := streakNotice(streak); !strings.Contains(notice, "TKT-9") || !strings.Contains(notice, "3 回連続") || !strings.Contains(notice, "AI の応答が得られず") {
		t.Fatalf("notice = %q", notice)
	}
}

// A delivery that needed a different plan and ran out of room to make one
// can end two ways. They are one thing going wrong, and the hold that stops
// intake counts them as one: three broken deliveries in a row hold the
// intake whichever of the two each ended as (review of #201).
func TestTheTwoDesignEndingsAreOneFailureForTheHold(t *testing.T) {
	never := func(state.RunOverview) bool { return false }
	run := func(id string, claimed int64, code string) state.RunOverview {
		return state.RunOverview{RunID: id, DeliveryID: id, State: "terminal", ClaimedAt: claimed, TerminalCode: code}
	}
	nonconverged := string(hook.TerminalDesignNonconverged)
	roundsSpent := string(hook.TerminalDesignRoundsSpent)

	mixed := detectFailureStreak([]state.RunOverview{
		run("a", 1, nonconverged), run("b", 2, roundsSpent), run("c", 3, nonconverged),
	}, 3, never)
	if !mixed.Active || mixed.Count != 3 {
		t.Fatalf("three design failures in a row did not hold intake: count=%d active=%v", mixed.Count, mixed.Active)
	}
	if mixed.Newest.RunID != "c" {
		t.Errorf("the notice would go to %q, not the newest failure", mixed.Newest.RunID)
	}
	// Two unrelated failures still end the count where they differ.
	apart := detectFailureStreak([]state.RunOverview{
		run("a", 1, nonconverged), run("b", 2, string(hook.TerminalModelFailed)), run("c", 3, nonconverged),
	}, 3, never)
	if apart.Active || apart.Count != 1 {
		t.Fatalf("unrelated failures were counted together: count=%d active=%v", apart.Count, apart.Active)
	}
}

// A run of failures that ended two different ways is one problem, and the
// notice says that. Naming the newest ending made the operator look for a
// disagreement two of the three runs never had (review of #201).
func TestAMixedStreakDoesNotClaimTheEndingsWereTheSame(t *testing.T) {
	never := func(state.RunOverview) bool { return false }
	run := func(id string, claimed int64, code string) state.RunOverview {
		return state.RunOverview{RunID: id, DeliveryID: id, State: "terminal", ClaimedAt: claimed, TerminalCode: code}
	}
	nonconverged := string(hook.TerminalDesignNonconverged)
	roundsSpent := string(hook.TerminalDesignRoundsSpent)

	mixed := detectFailureStreak([]state.RunOverview{
		run("a", 1, roundsSpent), run("b", 2, roundsSpent), run("c", 3, nonconverged),
	}, 3, never)
	if !mixed.Mixed {
		t.Fatal("a run of two different endings was not recognised as mixed")
	}
	posted := streakHoldContent(mixed)
	if strings.Contains(posted, "同じ結果") {
		t.Errorf("the notice claims the endings were the same: %q", posted)
	}
	if !strings.Contains(posted, "設計の段が") {
		t.Errorf("the notice does not say what the three runs had in common: %q", posted)
	}
	if strings.Contains(posted, "設計のレビューが収束せず終了") {
		t.Errorf("the notice names one ending as though all three ended that way: %q", posted)
	}
	// An operator's next move is to look the failures up, so the notice
	// carries the codes and how many ended each way (review of #201).
	if !strings.Contains(posted, "design_rounds_spent 2 件") || !strings.Contains(posted, "design_nonconverged 1 件") {
		t.Errorf("the notice does not say how the three runs ended: %q", posted)
	}
	if strings.Contains(posted, "受付停止（同じ失敗の連続）") {
		t.Errorf("the seven-item block still calls a mixed run the same failure: %q", posted)
	}
	if banner := streakNotice(mixed); strings.Contains(banner, "設計のレビューが収束せず終了") {
		t.Errorf("the board's banner names one ending: %q", banner)
	}

	// Three of the same ending still read as they did.
	same := detectFailureStreak([]state.RunOverview{
		run("a", 1, nonconverged), run("b", 2, nonconverged), run("c", 3, nonconverged),
	}, 3, never)
	if same.Mixed {
		t.Fatal("three identical endings were called mixed")
	}
	if posted := streakHoldContent(same); !strings.Contains(posted, "同じ結果") {
		t.Errorf("an unmixed streak lost its own words: %q", posted)
	}
}
