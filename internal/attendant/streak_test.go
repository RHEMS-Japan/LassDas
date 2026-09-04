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
