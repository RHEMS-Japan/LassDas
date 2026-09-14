package attendant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

// A queued ticket is told about the pause exactly once: the first tick posts
// the notice and records it; later ticks neither read nor write the ticket.
// A post that failed is not recorded, so it is retried.
func TestAQueuedTicketIsToldAboutThePauseOnce(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "d3")
	run := state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", IssueID: 30, State: "queued"}
	since := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	source := &fakeConfirmationSource{addErr: errors.New("tracker down")}
	noticeIntakePaused(context.Background(), source, run, since, runDir, resolutionTestLogger{})
	if _, err := os.Stat(filepath.Join(runDir, intakePausedNoticeFile)); err == nil {
		t.Fatal("a failed post must not be recorded as delivered")
	}
	source.addErr = nil
	noticeIntakePaused(context.Background(), source, run, since, runDir, resolutionTestLogger{})
	if len(source.added) != 1 || hook.ExtractCommentMarker(source.added[0]) != hook.CommentMarker(string(hook.RunCommentIntakePaused), "TKT-3") {
		t.Fatalf("notices = %d", len(source.added))
	}
	listings := source.listings
	noticeIntakePaused(context.Background(), source, run, since, runDir, resolutionTestLogger{})
	if len(source.added) != 1 || source.listings != listings {
		t.Fatalf("the told ticket must not be read or written again; notices = %d listings = %d", len(source.added), source.listings-listings)
	}
	// A notice already on the ticket (the record file lost) is found, not
	// posted twice.
	if err := os.Remove(filepath.Join(runDir, intakePausedNoticeFile)); err != nil {
		t.Fatal(err)
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 5, UserID: 1, Body: source.added[0]})
	noticeIntakePaused(context.Background(), source, run, since, runDir, resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("the notice was posted twice: %d", len(source.added))
	}
}

// While intake is paused the board says so — on the queued run and as the
// banner — and a claimed run is classified exactly as before.
func TestTheBoardShowsTheIntakePause(t *testing.T) {
	var config runtime.Config
	config.Chain.RunsRoot = t.TempDir()
	config.Chain.IntakePausedSince = "2026-09-14T09:00:00+09:00"
	queued := classifyRun(config, state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", State: "queued"}, nil)
	if queued.Step != "intake" || queued.StepTitle != "受付停止中" || !strings.Contains(queued.Detail, "2026-09-14") {
		t.Fatalf("queued run under the pause = step %q title %q detail %q", queued.Step, queued.StepTitle, queued.Detail)
	}
	since, _ := config.Chain.IntakePaused()
	if notice := intakePausedNotice(since); !strings.Contains(notice, "受付停止中") || !strings.Contains(notice, "実行中の依頼はそのまま進みます") {
		t.Fatalf("banner = %q", notice)
	}
	config.Chain.IntakePausedSince = ""
	resumed := classifyRun(config, state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", State: "queued"}, nil)
	if resumed.StepTitle != "受付待ち" {
		t.Fatalf("queued run without the pause = %q", resumed.StepTitle)
	}
}
