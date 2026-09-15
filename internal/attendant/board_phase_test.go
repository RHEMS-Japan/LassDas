package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

type recordingBoard struct {
	phases []hook.BoardPhase
	err    error
}

func (b *recordingBoard) ProjectBoardPhase(_ context.Context, _ int64, phase hook.BoardPhase) error {
	if b.err != nil {
		return b.err
	}
	b.phases = append(b.phases, phase)
	return nil
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
	config := runtime.Config{Chain: runtime.ChainConfig{RunsRoot: t.TempDir()}}
	deliveryID := "delivery_" + "1234567890abcdef1234567890abcdef"
	runDir := runDirectory(config, deliveryID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	run := state.RunOverview{RunID: "TKT-781", DeliveryID: deliveryID, IssueID: 781, TerminalCode: string(hook.TerminalSuccess)}
	board := &recordingBoard{}
	services := &runtime.Services{Board: board}
	logger := &pendingTestLogger{}

	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 0 {
		t.Fatalf("a run with no posted end was projected: %v", board.phases)
	}

	sealBoardOutcome(runDir, "staging", "deploy_not_applicable", "")
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	projectDeliveryEnd(context.Background(), config, services, run, nil, logger)
	if len(board.phases) != 1 || board.phases[0] != hook.BoardDelivered {
		t.Fatalf("phases = %v, want delivered exactly once", board.phases)
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
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: failing}, other, nil, logger)
	if _, ok := readBoardPhase(otherDir); ok {
		t.Fatal("a failed projection was recorded")
	}
	failing.err = nil
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: failing}, other, nil, logger)
	if len(failing.phases) != 1 || failing.phases[0] != hook.BoardNeedsAttention {
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
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention}, third, nil, logger)
	resolution, _ := json.Marshal(deliverResolution{Phase: "staging", Verdict: "deploy_absent", CommentID: 1, UserID: 1, At: time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(thirdDir, deliverResolutionFile), resolution, 0o600); err != nil {
		t.Fatal(err)
	}
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention}, third, nil, logger)
	projectDeliveryEnd(context.Background(), config, &runtime.Services{Board: attention}, third, nil, logger)
	if len(attention.phases) != 2 || attention.phases[0] != hook.BoardNeedsAttention || attention.phases[1] != hook.BoardDelivered {
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
