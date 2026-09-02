package attendant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

type fakeConfirmationSource struct {
	comments []hook.BacklogComment
	listErr  error
	addErr   error
	added    []string
}

func (f *fakeConfirmationSource) ListComments(context.Context, int64, int64) ([]hook.BacklogComment, error) {
	return f.comments, f.listErr
}

func (f *fakeConfirmationSource) AddComment(_ context.Context, _ int64, content string) (int64, error) {
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.added = append(f.added, content)
	return int64(1000 + len(f.added)), nil
}

type resolutionTestLogger struct{}

func (resolutionTestLogger) Info(string, ...any)  {}
func (resolutionTestLogger) Error(string, ...any) {}

func TestOperatorConfirmationFollowsTheStopRules(t *testing.T) {
	tracker := runtime.TrackerConfig{AllowedCreatorID: 7001, OperatorUserIDs: []int64{7002}}
	const report = int64(10)
	cases := map[string]struct {
		comments []hook.BacklogComment
		want     bool
	}{
		"requester after the report":       {[]hook.BacklogComment{{CommentID: 11, UserID: 7001, Body: "確認済み"}}, true},
		"listed operator after the report": {[]hook.BacklogComment{{CommentID: 11, UserID: 7002, Body: "  確認済み  \n本番は明日入れます"}}, true},
		"stranger":                         {[]hook.BacklogComment{{CommentID: 11, UserID: 7003, Body: "確認済み"}}, false},
		"before the report":                {[]hook.BacklogComment{{CommentID: 9, UserID: 7001, Body: "確認済み"}}, false},
		"the report itself":                {[]hook.BacklogComment{{CommentID: 10, UserID: 7001, Body: "確認済み"}}, false},
		"mention further down":             {[]hook.BacklogComment{{CommentID: 11, UserID: 7001, Body: "見ました\n確認済み"}}, false},
		"a different word":                 {[]hook.BacklogComment{{CommentID: 11, UserID: 7001, Body: "確認済"}}, false},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, got := operatorConfirmation(testCase.comments, tracker, report)
			if got != testCase.want {
				t.Fatalf("operatorConfirmation() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestResolveAttentionPostsOnceAndSeals(t *testing.T) {
	runDir := t.TempDir()
	run := state.RunOverview{RunID: "TKT-1", DeliveryID: "d1", IssueID: 42}
	tracker := runtime.TrackerConfig{AllowedCreatorID: 7001}
	reportMarker := hook.CommentMarker(string(hook.RunCommentStagingReport), run.RunID)
	source := &fakeConfirmationSource{comments: []hook.BacklogComment{
		{CommentID: 10, UserID: 7001, Body: "【ステージング反映が未確認】\n" + reportMarker},
		{CommentID: 11, UserID: 7001, Body: "確認済み"},
	}}
	resolveAttention(context.Background(), tracker, source, run, runDir, "staging", "deploy_failed", time.Now(), resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("acknowledgements posted = %d, want 1", len(source.added))
	}
	if hook.ExtractCommentMarker(source.added[0]) != hook.CommentMarker(string(hook.RunCommentResolved), run.RunID) {
		t.Fatalf("acknowledgement marker = %q", hook.ExtractCommentMarker(source.added[0]))
	}
	resolution, ok := readDeliverResolution(runDir)
	if !ok || resolution.Phase != "staging" || resolution.Verdict != "deploy_failed" || resolution.CommentID != 11 || resolution.UserID != 7001 {
		t.Fatalf("resolution = %+v (%v)", resolution, ok)
	}

	// The next tick sees the seal and does nothing.
	resolveAttention(context.Background(), tracker, source, run, runDir, "staging", "deploy_failed", time.Now(), resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("acknowledgements posted after the seal = %d, want 1", len(source.added))
	}

	// A crash between the post and the seal leaves the acknowledgement on
	// the ticket and no file: the marker stops a second post, the seal is
	// written from the comment that is already there.
	if err := os.Remove(filepath.Join(runDir, deliverResolutionFile)); err != nil {
		t.Fatal(err)
	}
	source.comments = append(source.comments, hook.BacklogComment{CommentID: 12, UserID: 1, Body: source.added[0]})
	resolveAttention(context.Background(), tracker, source, run, runDir, "staging", "deploy_failed", time.Now(), resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("acknowledgements posted after the crash replay = %d, want 1", len(source.added))
	}
	if _, ok := readDeliverResolution(runDir); !ok {
		t.Fatal("the seal was not rewritten from the posted acknowledgement")
	}
}

func TestResolveAttentionStaysSilentWithoutAValidConfirmation(t *testing.T) {
	run := state.RunOverview{RunID: "TKT-2", DeliveryID: "d2", IssueID: 43}
	tracker := runtime.TrackerConfig{AllowedCreatorID: 7001}
	releaseMarker := hook.CommentMarker(string(hook.RunCommentReleaseReport), run.RunID)
	cases := map[string]*fakeConfirmationSource{
		"listing failure": {listErr: errors.New("tracker down"), comments: []hook.BacklogComment{
			{CommentID: 10, UserID: 7001, Body: releaseMarker}, {CommentID: 11, UserID: 7001, Body: "確認済み"},
		}},
		"no report on the ticket yet": {comments: []hook.BacklogComment{{CommentID: 11, UserID: 7001, Body: "確認済み"}}},
		"a stranger's word": {comments: []hook.BacklogComment{
			{CommentID: 10, UserID: 7001, Body: releaseMarker}, {CommentID: 11, UserID: 7009, Body: "確認済み"},
		}},
		"the staging report does not answer for the release phase": {comments: []hook.BacklogComment{
			{CommentID: 10, UserID: 7001, Body: hook.CommentMarker(string(hook.RunCommentStagingReport), run.RunID)},
			{CommentID: 11, UserID: 7001, Body: "確認済み"},
		}},
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			runDir := t.TempDir()
			resolveAttention(context.Background(), tracker, source, run, runDir, "release", "merge_unverified", time.Now(), resolutionTestLogger{})
			if len(source.added) != 0 {
				t.Fatalf("acknowledgements posted = %d, want 0", len(source.added))
			}
			if _, ok := readDeliverResolution(runDir); ok {
				t.Fatal("a resolution was sealed without a valid confirmation")
			}
		})
	}

	// A failed post seals nothing: the next tick retries the post first.
	runDir := t.TempDir()
	source := &fakeConfirmationSource{addErr: errors.New("post refused"), comments: []hook.BacklogComment{
		{CommentID: 10, UserID: 7001, Body: releaseMarker}, {CommentID: 11, UserID: 7001, Body: "確認済み"},
	}}
	resolveAttention(context.Background(), tracker, source, run, runDir, "release", "merge_unverified", time.Now(), resolutionTestLogger{})
	if _, ok := readDeliverResolution(runDir); ok {
		t.Fatal("a resolution was sealed although the acknowledgement was never posted")
	}
}

func TestResolveAttentionStopsPollingAfterTheWindow(t *testing.T) {
	runDir := t.TempDir()
	run := state.RunOverview{RunID: "TKT-3", DeliveryID: "d3", IssueID: 44}
	tracker := runtime.TrackerConfig{AllowedCreatorID: 7001}
	reportMarker := hook.CommentMarker(string(hook.RunCommentStagingReport), run.RunID)
	source := &fakeConfirmationSource{comments: []hook.BacklogComment{
		{CommentID: 10, UserID: 7001, Body: reportMarker}, {CommentID: 11, UserID: 7001, Body: "確認済み"},
	}}
	stale := time.Now().Add(-operatorConfirmationWindow - time.Hour)
	resolveAttention(context.Background(), tracker, source, run, runDir, "staging", "deploy_failed", stale, resolutionTestLogger{})
	if len(source.added) != 0 {
		t.Fatalf("acknowledgements posted past the window = %d, want 0", len(source.added))
	}
	if _, ok := readDeliverResolution(runDir); ok {
		t.Fatal("a resolution was written past the window")
	}
	// A report with no timestamp (older artifacts) is still polled.
	resolveAttention(context.Background(), tracker, source, run, runDir, "staging", "deploy_failed", time.Time{}, resolutionTestLogger{})
	if len(source.added) != 1 {
		t.Fatalf("acknowledgements posted with no timestamp = %d, want 1", len(source.added))
	}
}

func TestReleaseAttentionVerdictFallsBackToTheProductionReport(t *testing.T) {
	runDir := t.TempDir()
	if _, _, waits := releaseAttentionVerdict(runDir); waits {
		t.Fatal("nothing on disk is not an attention state")
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("deliver-production-report.json", `{"phase":"production","verdict":"merge_unverified","observed_at":"2026-09-02T12:00:00Z"}`)
	verdict, since, waits := releaseAttentionVerdict(runDir)
	if !waits || verdict != "merge_unverified" || since.Year() != 2026 {
		t.Fatalf("report fallback = (%q, %v, %v)", verdict, since, waits)
	}
	write("board-outcome.json", `{"phase":"release","verdict":"deploy_failed","at":"2026-09-02T12:30:00Z"}`)
	if verdict, _, waits := releaseAttentionVerdict(runDir); !waits || verdict != "deploy_failed" {
		t.Fatalf("the seal must win over the file, got (%q, %v)", verdict, waits)
	}
	write("board-outcome.json", `{"phase":"release","verdict":"pass","at":"2026-09-02T12:30:00Z"}`)
	if _, _, waits := releaseAttentionVerdict(runDir); waits {
		t.Fatal("a passed release is not an attention state")
	}
}
