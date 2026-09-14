package hook

import (
	"strings"
	"testing"
	"time"
)

// A reminder repeats the two words that move the run and the deadline; a
// late-word reply says nothing resumes and how to continue. Both carry the
// marker the attendant looks for so each is posted once.
func TestWaitMessagesSayWhatMovesTheRunAndWhatDoesNot(t *testing.T) {
	deadline := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	reminder := GoReminderContent("TKT-7", 2, deadline)
	for _, want := range []string{"2 回目の確認", "「Go」", "何もしなくて構いません", "「停止」", "期限: 2026-09-21 09:00", "ステージングの変更はそのまま残ります", "次に行動する人: 依頼者"} {
		if !strings.Contains(reminder, want) {
			t.Errorf("the reminder lacks %q:\n%s", want, reminder)
		}
	}
	if ExtractCommentMarker(reminder) != GoReminderMarker("TKT-7", 2) || GoReminderMarker("TKT-7", 2) == GoReminderMarker("TKT-7", 3) {
		t.Fatalf("reminder marker = %q", ExtractCommentMarker(reminder))
	}
	late := LateWordContent("TKT-7", "Go")
	for _, want := range []string{"【期限切れで終了済み】", "この「Go」では自動処理は再開しません", "新しく起票", "そのまま残っています", "次に行動する人: 依頼者"} {
		if !strings.Contains(late, want) {
			t.Errorf("the late-word reply lacks %q:\n%s", want, late)
		}
	}
	for _, forbidden := range []string{"再開しました", "本番へ反映"} {
		if strings.Contains(late, forbidden) {
			t.Errorf("the late-word reply says %q, which is not what happens:\n%s", forbidden, late)
		}
	}
	if ExtractCommentMarker(late) != LateWordMarker("TKT-7") {
		t.Fatalf("late-word marker = %q", ExtractCommentMarker(late))
	}
	full := CommentMarker("terminal", "TKT-7", "clarification_expired", strings.Repeat("a", 64))
	if prefix := TerminalCommentMarkerPrefix("TKT-7", "clarification_expired"); !strings.HasPrefix(full, prefix) || strings.HasPrefix(CommentMarker("terminal", "TKT-7", "success", "x"), prefix) {
		t.Fatalf("terminal marker prefix %q does not select the code", prefix)
	}
}
