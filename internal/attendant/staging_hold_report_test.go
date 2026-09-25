package attendant

import (
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runner"
)

// A delivery held at staging tells the requester one thing, and it is true.
//
// The comment this covers used to say both「本番への反映は、下に書いた設定が
// 揃えば自動で行います」and「本番への反映は人が行います」in the same breath,
// for a run that was already terminal and would never issue another
// promotion card. It also called the hold a matter of settings when the
// hold was a production branch carrying changes staging does not have.
func TestAHeldStagingEndingPromisesNothingAutomatic(t *testing.T) {
	h := newDepthHarness(t, "production", true, "")
	h.write("readiness-ticket.json", `{"request":"一覧を絞り込めるようにする"}`)
	h.setBoard(h.card(deliverStageChecks, "done", 1), h.card(deliverStageIntegrate, "done", 1))
	h.write(runner.DeliverChecksFile, `{"ok":true}`)
	h.sealPhase(runner.DeliverStagingReportFile, h1StagingHold())

	h.tick() // posts the staging report
	h.tick() // reads the hold and ends the run

	row := h.runRow()
	if row.State != "terminal" || row.TerminalCode != string(hook.TerminalSuccess) {
		t.Fatalf("run = %s / %s, want a terminal success (log: %v)", row.State, row.TerminalCode, h.logger.lines)
	}
	if strings.Contains(h.calls(), "deliver:promote") {
		t.Fatalf("a held promotion ran: %s", h.calls())
	}
	comment := (*h.posted)[len(*h.posted)-1]

	// Nothing in a finished run reaches production later on its own.
	for _, promise := range []string{"設定が揃えば自動で", "自動で行います"} {
		if strings.Contains(comment, promise) {
			t.Errorf("the ending still promises production automatically (%q):\n%s", promise, comment)
		}
	}
	// And the same comment does not also hand the job to a person: two
	// instructions for one delivery is the contradiction this is about.
	if strings.Contains(comment, "本番への反映は人が行います") {
		t.Errorf("the ending gives a second, contradicting instruction:\n%s", comment)
	}
	// What it does say: the run is over and production was not reached.
	for _, said := range []string{
		"この実行はここで終わりで、本番へは届いていません",
		"この実行が後から自動で本番へ反映することはありません",
		"staging の表示をご確認ください（この実行は本番へ届いておらず、ここで終了しています）",
	} {
		if !strings.Contains(comment, said) {
			t.Errorf("the ending does not say %q:\n%s", said, comment)
		}
	}
	// The reason is the hold that actually held it, and it is not called a
	// missing setting.
	if !strings.Contains(comment, "ここまでで止まった理由: 本番にはステージングに無い変更が入っています（分岐状態）。") {
		t.Errorf("the stated reason is not the hold that held it:\n%s", comment)
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
