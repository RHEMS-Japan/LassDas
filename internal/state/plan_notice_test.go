package state

import (
	"context"
	"testing"
)

// The plan notice is a notice, not a gate, so a run continues without it -
// but the log line said only that it had not been posted. Live it appeared
// on every run, delivered and failed alike, and the reason was nowhere: the
// dispositions that are not errors return quietly. Now the caller is handed
// the reason to log.
func TestAPlanNoticeThatCannotBePostedSaysWhy(t *testing.T) {
	harness := newFlowHarness(t, newMemoryDynamo())
	ctx := context.Background()

	// Nothing is queued, so the notice has no run to attach itself to.
	posted, reason := harness.ticker.PostPlanComment(ctx, "delivery-1", "計画のお知らせ")
	if posted {
		t.Fatal("走行が無いのに投稿されたことになっています")
	}
	if reason == "" {
		t.Fatal("投稿できなかった理由が返っていません")
	}

	// With a run in flight the notice lands, and then says so without a
	// reason - a second call must not report a failure for an already
	// posted notice.
	claimForTerminal(t, harness.store)
	if posted, reason := harness.ticker.PostPlanComment(ctx, "delivery-1", "計画のお知らせ"); !posted || reason != "" {
		t.Fatalf("posted=%v reason=%q", posted, reason)
	}
	if posted, reason := harness.ticker.PostPlanComment(ctx, "delivery-1", "計画のお知らせ"); !posted || reason != "" {
		t.Fatalf("2 度目: posted=%v reason=%q", posted, reason)
	}
}
