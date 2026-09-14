package attendant

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// Reminders follow the questions' rhythm — the 1st, 3rd and 5th weekday
// after the report at 10:00 Asia/Tokyo — and stop at the deadline: a
// seven-day wait gets all three, a two-day wait only the first.
func TestGoRemindersFollowTheWeekdayRhythmAndStopAtTheDeadline(t *testing.T) {
	tokyo := hook.DisplayZone()
	observed := time.Date(2026, 9, 14, 15, 0, 0, 0, tokyo) // Monday
	sevenDays := observed.Add(7 * 24 * time.Hour)
	for _, tc := range []struct {
		name string
		now  time.Time
		dead time.Time
		want []int
	}{
		{"before the first instant", time.Date(2026, 9, 15, 9, 59, 0, 0, tokyo), sevenDays, nil},
		{"first weekday 10:00", time.Date(2026, 9, 15, 10, 0, 0, 0, tokyo), sevenDays, []int{1}},
		{"third weekday", time.Date(2026, 9, 17, 10, 0, 0, 0, tokyo), sevenDays, []int{1, 2}},
		{"fifth weekday", time.Date(2026, 9, 21, 10, 0, 0, 0, tokyo), sevenDays, []int{1, 2, 3}},
		{"a two-day wait stops after the first", time.Date(2026, 9, 21, 10, 0, 0, 0, tokyo), observed.Add(48 * time.Hour), []int{1}},
	} {
		got := dueGoReminders(tc.now, observed, tc.dead)
		if fmtInts(got) != fmtInts(tc.want) {
			t.Errorf("%s: due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func fmtInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.Itoa(v))
	}
	return strings.Join(parts, ",")
}

// A reminder is posted once per due instant, never again once its marker is
// on the ticket, and a post that fails stops the tick's reminders.
func TestGoRemindersArePostedOncePerInstant(t *testing.T) {
	run := state.RunOverview{RunID: "TKT-7", IssueID: 70}
	tokyo := hook.DisplayZone()
	observed := time.Now().In(tokyo).Add(-10 * 24 * time.Hour)
	deadline := time.Now().Add(24 * time.Hour)
	source := &fakeConfirmationSource{}
	remindGo(context.Background(), source, run, observed, deadline, nil, resolutionTestLogger{})
	if len(source.added) != 3 {
		t.Fatalf("reminders posted = %d, want 3 (all instants passed)", len(source.added))
	}
	var comments []hook.BacklogComment
	for i, body := range source.added {
		comments = append(comments, hook.BacklogComment{CommentID: int64(10 + i), UserID: 1, Body: body})
	}
	remindGo(context.Background(), source, run, observed, deadline, comments, resolutionTestLogger{})
	if len(source.added) != 3 {
		t.Fatalf("reminders reposted: %d", len(source.added))
	}
}

// A Go after the Go wait expired gets exactly one reply within the window,
// read at most once an hour; a Go before the report, or no Go, gets none.
func TestALateGoIsAnsweredOnceNotActedOn(t *testing.T) {
	var config runtime.Config
	config.Tracker.AllowedCreatorID = 7
	runDir := t.TempDir()
	run := state.RunOverview{RunID: "TKT-7", DeliveryID: "d7", IssueID: 70, State: "terminal"}
	sealBoardOutcome(runDir, "release", "expired", "")
	source := &fakeConfirmationSource{comments: []hook.BacklogComment{
		{CommentID: 1, UserID: 7, Body: "Go"}, // before the report: not late, not a Go for this wait
		{CommentID: 2, UserID: 1, Body: "expired\n" + hook.CommentMarker(string(hook.RunCommentReleaseReport), "TKT-7")},
	}}
	noticeLateGo(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if len(source.added) != 0 {
		t.Fatalf("a Go before the report was answered: %d", len(source.added))
	}
	os.Remove(filepath.Join(runDir, lateWordCheckFile))
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 3, UserID: 7, Body: "Go"})
	noticeLateGo(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if len(source.added) != 1 || hook.ExtractCommentMarker(source.added[0]) != hook.LateWordMarker("TKT-7") {
		t.Fatalf("late Go replies = %d", len(source.added))
	}
	listings := source.listings
	noticeLateGo(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if source.listings != listings {
		t.Fatal("the ticket was read again within the hour")
	}
	os.Remove(filepath.Join(runDir, lateWordCheckFile))
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 4, UserID: 1, Body: source.added[0]})
	noticeLateGo(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("the reply was posted twice: %d", len(source.added))
	}
	// A run whose release outcome is not "expired" is never read.
	other := t.TempDir()
	sealBoardOutcome(other, "release", "pass", "")
	before := source.listings
	noticeLateGo(context.Background(), config, source, run, other, resolutionTestLogger{})
	if source.listings != before {
		t.Fatal("a passed release was read for late words")
	}
}

// A requester comment after a question's deadline gets one reply; an
// operator's comment, or a comment before the terminal report, gets none.
func TestALateAnswerIsAnsweredOnce(t *testing.T) {
	var config runtime.Config
	config.Tracker.AllowedCreatorID = 7
	runDir := t.TempDir()
	run := state.RunOverview{RunID: "TKT-7", DeliveryID: "d7", IssueID: 70, State: "terminal",
		TerminalCode: string(hook.TerminalClarificationExpired), CompletedAt: time.Now().Add(-time.Hour).UnixMilli()}
	terminal := "expired\n" + hook.CommentMarker("terminal", "TKT-7", string(hook.TerminalClarificationExpired), strings.Repeat("a", 64))
	source := &fakeConfirmationSource{comments: []hook.BacklogComment{
		{CommentID: 1, UserID: 7, Body: "A"},
		{CommentID: 2, UserID: 1, Body: terminal},
		{CommentID: 3, UserID: 9, Body: "operator note"},
	}}
	noticeLateAnswer(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if len(source.added) != 0 {
		t.Fatalf("no requester word after the ending, yet a reply: %d", len(source.added))
	}
	os.Remove(filepath.Join(runDir, lateWordCheckFile))
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 4, UserID: 7, Body: "B"})
	noticeLateAnswer(context.Background(), config, source, run, runDir, resolutionTestLogger{})
	if len(source.added) != 1 || !strings.Contains(source.added[0], "「回答」") {
		t.Fatalf("late answer replies = %d", len(source.added))
	}
	// Outside the window nothing is read.
	old := run
	old.CompletedAt = time.Now().Add(-15 * 24 * time.Hour).UnixMilli()
	os.Remove(filepath.Join(runDir, lateWordCheckFile))
	before := source.listings
	noticeLateAnswer(context.Background(), config, source, old, runDir, resolutionTestLogger{})
	if source.listings != before {
		t.Fatal("a run finished two weeks ago was read")
	}
	// Other terminal codes are never read.
	success := run
	success.TerminalCode = string(hook.TerminalSuccess)
	os.Remove(filepath.Join(runDir, lateWordCheckFile))
	noticeLateAnswer(context.Background(), config, source, success, runDir, resolutionTestLogger{})
	if source.listings != before {
		t.Fatal("a successful run was read for late answers")
	}
}
