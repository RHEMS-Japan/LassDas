package attendant

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
)

// A held staging result is neither production nor a completed request.
// Do not claim completion, promise recovery not yet implemented, or label
// a diverged production branch as merely missing settings.
func TestAHeldStagingDeliveryDoesNotClaimCompletion(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	writeAcceptedReception(t, h, "一覧を絞り込めるようにする")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h1StagingHold())

	h.tick() // posts the staging report
	h.tick() // reads the hold without treating staging as the goal

	deliveryMustStillBeOpen(t, h)
	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("a held promotion ran: %s", h.calls())
	}
	comment := strings.Join(*h.posted, "\n")
	if strings.Contains(comment, hook.TerminalMarkerPrefix(depthRunID)) {
		t.Fatal("a held production delivery posted a final comment")
	}

	// Resolving a sealed promotion hold is not implemented yet.
	for _, promise := range []string{"設定が揃えば自動で", "自動で行います"} {
		if strings.Contains(comment, promise) {
			t.Errorf("the ending still promises production automatically (%q):\n%s", promise, comment)
		}
	}
	// Do not hand off a supposedly completed delivery to a person either.
	if strings.Contains(comment, "本番への反映は人が行います") {
		t.Errorf("the ending gives a second, contradicting instruction:\n%s", comment)
	}
	// The request is still open. A held route is not a completed delivery.
	for _, said := range []string{
		"この実行はここで終わりで、本番へは届いていません",
		"この実行が後から自動で本番へ反映することはありません",
		"staging の表示をご確認ください（この実行は本番へ届いておらず、ここで終了しています）",
	} {
		if strings.Contains(comment, said) {
			t.Errorf("the open delivery claims to have ended (%q):\n%s", said, comment)
		}
	}
	// The reason is the hold that actually held it, and it is not called a
	// missing setting.
	if log := strings.Join(h.logger.lines, "\n"); !strings.Contains(log, h1StagingHold().PromotionHold) {
		t.Errorf("the unfinished delivery lost its hold reason: %s", log)
	}
	if strings.Contains(comment, "そこまで運ぶ設定がこの環境に揃っていない") {
		t.Errorf("a diverged production branch is described as missing settings:\n%s", comment)
	}
}

// A destination that asked for staging and got there is not held, and its
// ending still says a person takes it further. The sentence above is the
// held case only.
func TestAStagingDestinationStillSaysAPersonPromotes(t *testing.T) {
	h := newDepthHarness(t, "integration", true, "")
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h.stagingPass())

	h.tick()
	h.tick()

	if row := h.runRow(); row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("run = %s / %s (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	comment := (*h.posted)[len(*h.posted)-1]
	if !strings.Contains(comment, "本番への反映は人が行います") {
		t.Errorf("a staging destination no longer says who promotes:\n%s", comment)
	}
	if strings.Contains(comment, "ここまでで止まった理由") {
		t.Errorf("a destination that reached its own depth was reported as short:\n%s", comment)
	}
}
