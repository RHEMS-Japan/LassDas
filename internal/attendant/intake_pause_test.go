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

func pausedConfig(t *testing.T, since string) runtime.Config {
	t.Helper()
	var config runtime.Config
	config.Chain.RunsRoot = t.TempDir()
	config.Chain.IntakePausedSince = since
	return config
}

// A queued run stays queued under the operator's pause, told once on its
// ticket, and starts when the pause is not in force.
func TestAQueuedRunIsHeldByThePause(t *testing.T) {
	run := state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", IssueID: 30, State: "queued"}
	source := &fakeConfirmationSource{}
	config := pausedConfig(t, "")
	runDir := filepath.Join(config.Chain.RunsRoot, "d3")
	if holdQueuedRun(context.Background(), config, source, run, runDir, resolutionTestLogger{}) {
		t.Fatal("nothing holds the run, yet it stayed queued")
	}
	config.Chain.IntakePausedSince = "2026-09-14T08:30:00+09:00"
	if !holdQueuedRun(context.Background(), config, source, run, runDir, resolutionTestLogger{}) || len(source.added) != 1 {
		t.Fatalf("the pause must hold and tell the ticket once; notices = %d", len(source.added))
	}
	since, _ := config.Chain.IntakePaused()
	if hook.ExtractCommentMarker(source.added[0]) != hook.IntakePausedMarker("TKT-3", since) {
		t.Fatalf("notice marker = %q", hook.ExtractCommentMarker(source.added[0]))
	}
	info, err := os.Stat(runDir)
	if err != nil || info.Mode().Perm() != 0o711 {
		t.Fatalf("run directory mode = %v err = %v, want 711 (the agent user must enter it later)", info.Mode(), err)
	}
}

// The notice goes out once per pause: a failed post is retried, a delivered
// one is neither read nor written again, a marker already on the ticket is
// found rather than posted twice — and a LATER pause, after the run came
// back to the queue, is told again.
func TestAQueuedTicketIsToldAboutEachPauseOnce(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "d3")
	run := state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", IssueID: 30, State: "queued"}
	first := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	source := &fakeConfirmationSource{addErr: errors.New("tracker down")}
	noticeIntakePaused(context.Background(), source, run, first, runDir, resolutionTestLogger{})
	if toldAbout(runDir, first) {
		t.Fatal("a failed post must not be recorded as delivered")
	}
	source.addErr = nil
	noticeIntakePaused(context.Background(), source, run, first, runDir, resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("notices = %d", len(source.added))
	}
	listings := source.listings
	noticeIntakePaused(context.Background(), source, run, first, runDir, resolutionTestLogger{})
	if len(source.added) != 1 || source.listings != listings {
		t.Fatalf("the told ticket must not be read or written again; notices = %d extra listings = %d", len(source.added), source.listings-listings)
	}
	// The record is lost (a claim empties the run directory) but the
	// notice is on the ticket: found, not posted twice.
	if err := os.Remove(filepath.Join(runDir, intakePausedNoticeFile)); err != nil {
		t.Fatal(err)
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 5, UserID: 1, Body: source.added[0]})
	noticeIntakePaused(context.Background(), source, run, first, runDir, resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("the same pause was told twice: %d", len(source.added))
	}
	// A later pause is a different fact and is told.
	second := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	noticeIntakePaused(context.Background(), source, run, second, runDir, resolutionTestLogger{})
	if len(source.added) != 2 || hook.ExtractCommentMarker(source.added[1]) != hook.IntakePausedMarker("TKT-3", second) {
		t.Fatalf("the later pause was not told; notices = %d", len(source.added))
	}
}

// While intake is paused the board says so — on the queued run, ahead of a
// stale budget or login hold, and as the banner — with the instant in the
// display zone that the ticket uses, whatever the pod's local zone is. A
// claimed run is classified exactly as before.
func TestTheBoardShowsTheIntakePause(t *testing.T) {
	config := pausedConfig(t, "2026-09-14T08:30:00+09:00")
	queued := classifyRun(config, state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", State: "queued"}, nil)
	if queued.Step != "intake" || queued.StepTitle != "受付停止中" || !strings.Contains(queued.Detail, "2026-09-14 08:30") {
		t.Fatalf("queued run under the pause = step %q title %q detail %q", queued.Step, queued.StepTitle, queued.Detail)
	}
	if notice := intakeNotice(config); !strings.Contains(notice, "受付停止中") || !strings.Contains(notice, "2026-09-14 08:30") || !strings.Contains(notice, "実行中の依頼はそのまま進みます") {
		t.Fatalf("banner = %q", notice)
	}
	config.Chain.IntakePausedSince = ""
	if notice := intakeNotice(config); notice != "" {
		t.Fatalf("banner without a hold = %q", notice)
	}
	resumed := classifyRun(config, state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", State: "queued"}, nil)
	if resumed.StepTitle != "受付待ち" {
		t.Fatalf("queued run without the pause = %q", resumed.StepTitle)
	}
}
