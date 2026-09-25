package runner

import (
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/worker"
)

// sealedGapPlan is a delivery whose destination asked for a depth it had no
// path to, with one part the engine built and two it was handed no means
// for.
func sealedGapPlan(t *testing.T, runDir string) {
	t.Helper()
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion,
		Repository:    "example/consumer", Configured: "production",
		DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []worker.ReleasePathItem{
			{Name: "chain.deliver.promote_profile", Kind: worker.ReleasePathCards,
				Detail: "納品を運ぶカードの設定です。", Means: "本体が自分の実行権限を書き換えることはしません。"},
			{Name: "production_origin", Kind: worker.ReleasePathOrigin,
				Detail: "本番が応答する場所です。", Means: "納品先の環境そのものの値なので、本体には決められません。"},
			{Name: "反映を画面から確かめる入口", Kind: worker.ReleasePathObservation,
				Detail: "画面から確かめられる入口を用意してください。"},
		},
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	writeOutcomeRecord(t, runDir, worker.ReleasePathFile, plan)
}

// The settings the engine was not handed the means to write reach the
// requester as facts about what this delivery did not touch, under the keys
// an operator knows them by — never as a line telling somebody to go and
// configure something, which is the shape the release path work exists to
// remove.
func TestTheOutcomeNamesWhatTheDeliveryDidNotApply(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "ticket.json", map[string]any{"request": "画面の文言を直す"})
	sealedGapPlan(t, runDir)

	text, _ := composeOutcome(runDir, hook.TerminalSuccess, map[string]string{
		"reached_delivery": "integration",
		"pull_request_url": "https://example.test/pull/1",
	})

	for _, key := range []string{"chain.deliver.promote_profile", "production_origin"} {
		if !strings.Contains(text, key) {
			t.Fatalf("the outcome does not name %q: %s", key, text)
		}
	}
	// A part the engine built is not something it left undone.
	if strings.Contains(text, "反映を画面から確かめる入口") {
		t.Fatalf("a part the engine built was reported as unapplied: %s", text)
	}
	// No request to a person: the section states what was not touched.
	for _, asking := range []string{"設定してください", "お願いします", "ご対応", "設定をお願い"} {
		if strings.Contains(text, asking) {
			t.Fatalf("the outcome asks a person to configure something: %s", text)
		}
	}
}

// A delivery with no plan says nothing about unapplied settings: most
// destinations have a complete path or stop at the proposal, and a section
// about nothing reads as a problem that is not there.
func TestADeliveryWithNoPlanSaysNothingAboutUnappliedSettings(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "ticket.json", map[string]any{"request": "画面の文言を直す"})

	text, _ := composeOutcome(runDir, hook.TerminalSuccess, map[string]string{
		"reached_delivery": "production",
	})
	if strings.Contains(text, "適用していない設定") {
		t.Fatalf("a delivery with no plan reported unapplied settings: %s", text)
	}
}

// A plan that is there and will not read is named with the other records
// the report could not read. An absent section reads as "nothing was left
// undone", which is a claim this report has no business making on that
// evidence.
func TestAnUnreadablePlanIsNamedRatherThanPassedOver(t *testing.T) {
	runDir := t.TempDir()
	writeOutcomeRecord(t, runDir, "ticket.json", map[string]any{"request": "画面の文言を直す"})
	writeOutcomeRecord(t, runDir, worker.ReleasePathFile, "{not a plan")

	text, _ := composeOutcome(runDir, hook.TerminalSuccess, map[string]string{
		"reached_delivery": "production",
	})
	if !strings.Contains(text, recordPath) {
		t.Fatalf("an unreadable plan was passed over in silence: %s", text)
	}
}
