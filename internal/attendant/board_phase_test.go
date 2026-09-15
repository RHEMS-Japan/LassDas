package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

type recordingBoard struct {
	// phases lists every projection as "<issue id>:<phase>", so a test pins
	// which ticket was moved, not only to what.
	phases []string
	err    error
}

func (b *recordingBoard) ProjectBoardPhase(_ context.Context, issueID int64, phase hook.BoardPhase) error {
	if b.err != nil {
		return b.err
	}
	b.phases = append(b.phases, fmt.Sprintf("%d:%s", issueID, phase))
	return nil
}

// boardTestStatuses are the tracker statuses the tests treat as the
// automation's own.
var boardTestStatuses = runtime.BoardStatuses{Running: 11, AwaitingAnswer: 12, Delivered: 13, NeedsAttention: 14}

// statusClient is a tracker client whose issue reads answer the given
// status id for every issue.
func statusClient(t *testing.T, statusID int64) *backlog.Client {
	t.Helper()
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			id := strings.TrimPrefix(r.URL.Path, "/api/v2/issues/")
			body := fmt.Sprintf(`{"id":%s,"status":{"id":%d}}`, id, statusID)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// The tracker phase a run's end calls for follows the status board's step:
// done with nothing to do is delivered; done with an operator's action,
// failed, attention and stopped need attention; a run still moving (a Go
// wait) is nobody's to project here.
func TestDeliveryEndPhaseFollowsTheStatusBoard(t *testing.T) {
	for name, c := range map[string]struct {
		status RunStatus
		want   hook.BoardPhase
	}{
		"done":                  {RunStatus{Step: "done"}, hook.BoardDelivered},
		"done, nothing to do":   {RunStatus{Step: "done", NextAction: "対応不要です。"}, hook.BoardDelivered},
		"done, operator action": {RunStatus{Step: "done", NextAction: "運用担当者が確認してください。"}, hook.BoardNeedsAttention},
		"failed":                {RunStatus{Step: "failed"}, hook.BoardNeedsAttention},
		"attention":             {RunStatus{Step: "attention"}, hook.BoardNeedsAttention},
		"stopped":               {RunStatus{Step: "stopped"}, hook.BoardNeedsAttention},
		"awaiting Go":           {RunStatus{Step: "confirm"}, ""},
		"still moving":          {RunStatus{Step: "staging"}, ""},
	} {
		if got := deliveryEndPhase(c.status); got != c.want {
			t.Errorf("%s: phase %q, want %q", name, got, c.want)
		}
	}
	// The placements the status board makes for the staging verdicts land
	// where the ticket must go.
	var status RunStatus
	placeStagingOutcome(&status, "deploy_not_applicable", "")
	if deliveryEndPhase(status) != hook.BoardDelivered {
		t.Fatalf("deploy_not_applicable at staging: step %q phase %q, want delivered", status.Step, deliveryEndPhase(status))
	}
	status = RunStatus{}
	placeStagingOutcome(&status, "pass", "")
	if deliveryEndPhase(status) != "" {
		t.Fatalf("a staging pass awaiting Go must not be projected, got %q", deliveryEndPhase(status))
	}
	status = RunStatus{}
	placeStagingOutcome(&status, "pass", "本番反映は手動")
	if deliveryEndPhase(status) != hook.BoardNeedsAttention {
		t.Fatalf("a staging pass held from promotion asks the operator: got %q", deliveryEndPhase(status))
	}
	status = RunStatus{}
	placeReleaseOutcome(&status, "pass")
	if deliveryEndPhase(status) != hook.BoardDelivered {
		t.Fatalf("a production pass is delivered, got %q", deliveryEndPhase(status))
	}
}

// A delivered run whose staging report ended it (here: merged, no deploy
// applicable) is projected as delivered once; the next tick sees the record
// and does not project again; a failed projection leaves no record so the
// next tick tries again; a run whose end is not posted yet is left alone.
func TestProjectDeliveryEndProjectsOncePerPhase(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}, Tracker: runtime.TrackerConfig{BoardStatuses: boardTestStatuses}}
	deliveryID := "delivery_" + "1234567890abcdef1234567890abcdef"
	runDir := runDirectory(config, deliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	run := state.RunOverview{RunID: "TKT-781", DeliveryID: deliveryID, IssueID: 781, TerminalCode: string(hook.TerminalSuccess)}
	board := &recordingBoard{}
	owned := statusClient(t, boardTestStatuses.Running)
	services := &runtime.Services{Board: board, Backlog: owned}
	logger := &pendingTestLogger{}

	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 0 {
		t.Fatalf("a run with no posted end was projected: %v", board.phases)
	}

	sealBoardOutcome(runDir, "staging", "deploy_not_applicable", "")
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 1 || board.phases[0] != "781:delivered" {
		t.Fatalf("phases = %v, want 781:delivered exactly once", board.phases)
	}
	record, ok := readBoardPhase(runDir)
	if !ok || record.Phase != string(hook.BoardDelivered) || record.At.IsZero() {
		t.Fatalf("record = %+v %v, want the delivered phase recorded", record, ok)
	}

	// A failed projection is tried again next tick.
	failing := &recordingBoard{err: errors.New("tracker down")}
	other := state.RunOverview{RunID: "TKT-782", DeliveryID: "delivery_" + "abcdef1234567890abcdef1234567890", IssueID: 782, TerminalCode: string(hook.TerminalSuccess)}
	otherDir := runDirectory(config, other.DeliveryID)
	if err := os.MkdirAll(otherDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(otherDir, "staging", "stopped", "")
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: failing, Backlog: owned}, other, nil, logger)
	if _, ok := readBoardPhase(otherDir); ok {
		t.Fatal("a failed projection was recorded")
	}
	failing.err = nil
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: failing, Backlog: owned}, other, nil, logger)
	if len(failing.phases) != 1 || failing.phases[0] != "782:needs_attention" {
		t.Fatalf("stopped run phases = %v, want needs attention once the tracker answers", failing.phases)
	}

	// An operator's 「確認済み」 after an attention report changes the end,
	// and the new phase is projected once more.
	third := state.RunOverview{RunID: "TKT-783", DeliveryID: "delivery_" + "0000000000abcdef0000000000abcdef", IssueID: 783, TerminalCode: string(hook.TerminalSuccess)}
	thirdDir := runDirectory(config, third.DeliveryID)
	if err := os.MkdirAll(thirdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(thirdDir, "staging", "deploy_absent", "")
	attention := &recordingBoard{}
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention, Backlog: owned}, third, nil, logger)
	resolution, _ := json.Marshal(deliverResolution{Phase: "staging", Verdict: "deploy_absent", CommentID: 1, UserID: 1, At: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(thirdDir, deliverResolutionFile), resolution, 0o600); err != nil {
		t.Fatal(err)
	}
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention, Backlog: owned}, third, nil, logger)
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention, Backlog: owned}, third, nil, logger)
	if len(attention.phases) != 2 || attention.phases[0] != "783:needs_attention" || attention.phases[1] != "783:delivered" {
		t.Fatalf("attention then resolved phases = %v, want needs attention then delivered", attention.phases)
	}

	// No board configured: nothing happens and nothing is recorded.
	fourth := state.RunOverview{RunID: "TKT-784", DeliveryID: "delivery_" + "1111111111abcdef1111111111abcdef", IssueID: 784, TerminalCode: string(hook.TerminalSuccess)}
	fourthDir := runDirectory(config, fourth.DeliveryID)
	_ = os.MkdirAll(fourthDir, 0o700)
	sealBoardOutcome(fourthDir, "staging", "deploy_not_applicable", "")
	projectDeliveryEnd(context.Background(), config, &runtime.Services{}, fourth, nil, logger)
	if _, ok := readBoardPhase(fourthDir); ok {
		t.Fatal("a projection was recorded without a board")
	}
}

// A production report file whose release report is not posted yet is the
// previous story still: nothing is projected until the release seal exists,
// and then the release's own end is.
func TestProjectDeliveryEndWaitsForThePostedReleaseReport(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}, Tracker: runtime.TrackerConfig{BoardStatuses: boardTestStatuses}}
	run := state.RunOverview{RunID: "TKT-790", DeliveryID: "delivery_" + "2222222222abcdef2222222222abcdef", IssueID: 790, TerminalCode: string(hook.TerminalSuccess)}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	board := &recordingBoard{}
	services := &runtime.Services{Board: board, Backlog: statusClient(t, boardTestStatuses.Running)}
	logger := &pendingTestLogger{}
	sealBoardOutcome(runDir, "staging", "pass", "")
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 0 {
		t.Fatalf("a staging pass awaiting Go was projected: %v", board.phases)
	}
	report, _ := json.Marshal(runner.DeliverReport{SchemaVersion: 1, Phase: "production", Verdict: "pass", ObservedAt: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(runDir, runner.DeliverProductionReportFile), report, 0o600); err != nil {
		t.Fatal(err)
	}
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 0 {
		t.Fatalf("a production report not yet posted was projected: %v", board.phases)
	}
	sealBoardOutcome(runDir, "release", "pass", "")
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 1 || board.phases[0] != "790:delivered" {
		t.Fatalf("phases = %v, want 790:delivered once the release report is posted", board.phases)
	}
}

// A ticket that is not in one of the automation's own statuses - one a
// person closed - is left alone, and remembered as left alone so the
// tracker is not asked on every tick; a ticket in an automation status is
// moved.
func TestProjectDeliveryEndLeavesTicketsItDoesNotOwn(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}, Tracker: runtime.TrackerConfig{BoardStatuses: boardTestStatuses}}
	run := state.RunOverview{RunID: "TKT-791", DeliveryID: "delivery_" + "3333333333abcdef3333333333abcdef", IssueID: 791, TerminalCode: string(hook.TerminalSuccess)}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(runDir, "staging", "deploy_not_applicable", "")
	board := &recordingBoard{}
	closedByHand := statusClient(t, 4) // 完了: not one of the four
	logger := &pendingTestLogger{}
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: board, Backlog: closedByHand}, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: board, Backlog: closedByHand}, run, nil, logger)
	if len(board.phases) != 0 {
		t.Fatalf("a ticket closed by hand was moved: %v", board.phases)
	}
	record, ok := readBoardPhase(runDir)
	if !ok || record.Phase != untouchedRecord(hook.BoardDelivered) {
		t.Fatalf("record = %+v %v, want the untouched phase remembered", record, ok)
	}
	for _, owned := range []int64{boardTestStatuses.Running, boardTestStatuses.AwaitingAnswer, boardTestStatuses.NeedsAttention, boardTestStatuses.Delivered} {
		if !boardOwned(owned, boardTestStatuses) {
			t.Errorf("status %d is the automation's own", owned)
		}
	}
	if boardOwned(0, boardTestStatuses) || boardOwned(4, boardTestStatuses) {
		t.Error("a status outside the four was taken as owned")
	}
}

// The release endings that write no production report file - a stop or an
// expiry during the Go wait, a dead promote card - are sealed as release
// once their report is posted, and are projected from that seal; an
// operator's 「確認済み」 on an attention ending then moves the ticket on.
func TestProjectDeliveryEndProjectsReleaseSealsWithoutAReportFile(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}, Tracker: runtime.TrackerConfig{BoardStatuses: boardTestStatuses}}
	logger := &pendingTestLogger{}
	for i, c := range []struct {
		verdict string
		want    string
	}{
		{"expired", "needs_attention"}, {"stopped", "needs_attention"}, {"card_failed", "needs_attention"},
		{"deploy_failed", "needs_attention"}, {"merge_unverified", "needs_attention"},
	} {
		run := state.RunOverview{RunID: fmt.Sprintf("TKT-80%d", i), DeliveryID: fmt.Sprintf("delivery_%032d", i), IssueID: int64(800 + i), TerminalCode: string(hook.TerminalSuccess)}
		runDir := runDirectory(config, run.DeliveryID)
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatal(err)
		}
		sealBoardOutcome(runDir, "staging", "pass", "")
		sealBoardOutcome(runDir, "release", c.verdict, "")
		board := &recordingBoard{}
		projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: board, Backlog: statusClient(t, boardTestStatuses.Running)}, run, nil, logger)
		if want := fmt.Sprintf("%d:%s", run.IssueID, c.want); len(board.phases) != 1 || board.phases[0] != want {
			t.Errorf("release %s without a report file: phases %v, want %s", c.verdict, board.phases, want)
		}
	}
	// deploy_failed then the operator's 確認済み: attention, then delivered.
	run := state.RunOverview{RunID: "TKT-810", DeliveryID: fmt.Sprintf("delivery_%032d", 10), IssueID: 810, TerminalCode: string(hook.TerminalSuccess)}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(runDir, "release", "deploy_failed", "")
	board := &recordingBoard{}
	services := &runtime.Services{Board: board, Backlog: statusClient(t, boardTestStatuses.Running)}
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	resolution, _ := json.Marshal(deliverResolution{Phase: "release", Verdict: "deploy_failed", CommentID: 1, UserID: 1, At: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(runDir, deliverResolutionFile), resolution, 0o600); err != nil {
		t.Fatal(err)
	}
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 2 || board.phases[0] != "810:needs_attention" || board.phases[1] != "810:delivered" {
		t.Fatalf("phases = %v, want needs attention then delivered", board.phases)
	}
}

// A ticket the tracker no longer has is left alone and remembered, like a
// ticket in a person's status; a tracker error is tried again next tick.
func TestProjectDeliveryEndLeavesADeletedTicketAlone(t *testing.T) {
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}, Tracker: runtime.TrackerConfig{BoardStatuses: boardTestStatuses}}
	run := state.RunOverview{RunID: "TKT-820", DeliveryID: fmt.Sprintf("delivery_%032d", 20), IssueID: 820, TerminalCode: string(hook.TerminalSuccess)}
	runDir := runDirectory(config, run.DeliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sealBoardOutcome(runDir, "staging", "deploy_not_applicable", "")
	reads := 0
	client := func(status int) *backlog.Client {
		c, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
			roundTripFunc(func(r *http.Request) (*http.Response, error) {
				reads++
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"errors":[]}`))}, nil
			}))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	board := &recordingBoard{}
	logger := &pendingTestLogger{}
	failing := &runtime.Services{Board: board, Backlog: client(500)}
	projectDeliveryEnd(context.Background(), config, failing, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, failing, run, nil, logger)
	if reads != 2 || len(board.phases) != 0 {
		t.Fatalf("a tracker error: reads=%d phases=%v, want two reads and no projection", reads, board.phases)
	}
	if _, ok := readBoardPhase(runDir); ok {
		t.Fatal("a tracker error was recorded as a phase")
	}
	// An authentication failure or a corrupt answer is not "the ticket is
	// gone": it is tried again, and never recorded as untouched (an
	// expired API key must not silence every run until its end changes).
	for _, status := range []int{401, 403} {
		reads = 0
		rejected := &runtime.Services{Board: board, Backlog: client(status)}
		projectDeliveryEnd(context.Background(), config, rejected, run, nil, logger)
		projectDeliveryEnd(context.Background(), config, rejected, run, nil, logger)
		if reads != 2 || len(board.phases) != 0 {
			t.Fatalf("HTTP %d: reads=%d phases=%v, want two reads and no projection", status, reads, board.phases)
		}
		if _, ok := readBoardPhase(runDir); ok {
			t.Fatalf("HTTP %d was recorded as a phase", status)
		}
	}
	reads = 0
	corrupt, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reads++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":999,"status":{"id":0}}`))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: board, Backlog: corrupt}, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: board, Backlog: corrupt}, run, nil, logger)
	if reads != 2 || len(board.phases) != 0 {
		t.Fatalf("a corrupt answer: reads=%d phases=%v, want two reads and no projection", reads, board.phases)
	}
	if _, ok := readBoardPhase(runDir); ok {
		t.Fatal("a corrupt answer was recorded as a phase")
	}
	reads = 0
	gone := &runtime.Services{Board: board, Backlog: client(404)}
	projectDeliveryEnd(context.Background(), config, gone, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, gone, run, nil, logger)
	record, ok := readBoardPhase(runDir)
	if reads != 1 || len(board.phases) != 0 || !ok || record.Phase != untouchedRecord(hook.BoardDelivered) {
		t.Fatalf("a deleted ticket: reads=%d phases=%v record=%+v %v, want one read, no projection, untouched recorded", reads, board.phases, record, ok)
	}
}
