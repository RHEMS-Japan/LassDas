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

	streak := FailureStreakContent("RUN-2", "model_failed", 3)
	if ExtractCommentMarker(streak) != CommentMarker("streak-hold", "RUN-2") {
		t.Fatalf("streak marker = %q", ExtractCommentMarker(streak))
	}
	for _, needle := range []string{"3 回連続", "AI の応答が得られず終了 (model_failed)", "「確認済み」", "受付停止"} {
		if !strings.Contains(streak, needle) {
			t.Fatalf("streak hold lacks %q:\n%s", needle, streak)
		}
	}

	resolved := StreakResolvedContent("RUN-2")
	if ExtractCommentMarker(resolved) != CommentMarker("streak-resolved", "RUN-2") || !strings.Contains(resolved, "受付を再開") {
		t.Fatalf("streak resolved:\n%s", resolved)
	}
	if DescribeTerminalCode("somewhere_new") != "somewhere_new" {
		t.Fatal("an unknown code must pass through unchanged")
	}
}
