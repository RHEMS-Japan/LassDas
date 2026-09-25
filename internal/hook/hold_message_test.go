package hook

import (
	"strings"
	"testing"
)

func TestHoldMessagesCarryTheirMarkersAndSpeakToTheRequester(t *testing.T) {
	budget := BudgetHoldContent("RUN-1", []string{"実装役", "レビュー役 A"})
	if ExtractCommentMarker(budget) != CommentMarker("budget-hold", "RUN-1") {
		t.Fatalf("budget marker = %q", ExtractCommentMarker(budget))
	}
	for _, needle := range []string{"実装役、レビュー役 A", "自動的に開始します", "本番の状態: 未変更", "自動再試行: あり"} {
		if !strings.Contains(budget, needle) {
			t.Fatalf("budget hold lacks %q:\n%s", needle, budget)
		}
	}

	// The waiting rung's two notices. Neither asks the requester for
	// anything and neither says the delivery ended, because it has not:
	// both say the work continues by itself.
	waiting := LadderWaitContent("RUN-2", "review-a")
	if ExtractCommentMarker(waiting) != CommentMarker("ladder", "RUN-2", "review-a", "wait") {
		t.Fatalf("ladder marker = %q", ExtractCommentMarker(waiting))
	}
	for _, needle := range []string{"処理は続いています", "依頼は止まっていません", "対応不要", "自動再試行: あり"} {
		if !strings.Contains(waiting, needle) {
			t.Fatalf("the waiting notice lacks %q:\n%s", needle, waiting)
		}
	}
	// It names no cause. Everything out of remedies ends on this rung — a
	// model that would not answer, a destination that refuses the change
	// every time, a failure nobody could name — so a sentence blaming an
	// outside service would be wrong for most of them.
	for _, blame := range []string{"外部のサービス", "外部サービス", "AI", "ネットワーク"} {
		if strings.Contains(waiting, blame) {
			t.Fatalf("the waiting notice blames %q, and this rung takes every kind of failure:\n%s", blame, waiting)
		}
	}

	limit := KeyLimitReachedContent("RUN-2", "implement")
	if ExtractCommentMarker(limit) != CommentMarker("ladder", "RUN-2", "implement", "wait") {
		t.Fatalf("key limit marker = %q", ExtractCommentMarker(limit))
	}
	for _, needle := range []string{"利用枠の上限", "依頼は止まっていません", "運用担当者", "自動再試行: あり"} {
		if !strings.Contains(limit, needle) {
			t.Fatalf("the key limit notice lacks %q:\n%s", needle, limit)
		}
	}
	if strings.Contains(waiting, "終了") || strings.Contains(limit, "終了") {
		t.Fatal("a waiting notice must not read as an ending")
	}
	if DescribeTerminalCode("somewhere_new") != "somewhere_new" {
		t.Fatal("an unknown code must pass through unchanged")
	}
}
