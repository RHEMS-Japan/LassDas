package hook

import (
	"fmt"
	"strconv"
	"time"
)

// GoReminderMarker names the n-th reminder of one run's Go wait.
func GoReminderMarker(runID string, n int) string {
	return CommentMarker(string(RunCommentGoReminder), runID, strconv.Itoa(n))
}

// GoReminderContent is the reminder a requester gets while the staging
// report waits for their Go — the same weekday rhythm the questions use.
// It repeats the two words that move the run and says what the deadline
// does, so a requester who missed the report can still act from here.
func GoReminderContent(runID string, n int, deadline time.Time) string {
	when := deadline.In(questionZone).Format("2006-01-02 15:04")
	body := fmt.Sprintf(
		"【本番反映の承認待ち（%d 回目の確認）】ステージングの確認は合格しています。本番へ反映するなら、このチケットに「Go」とだけコメントしてください（依頼者ご本人のコメントのみ有効）。反映しない場合は何もしなくて構いません（期限で本番反映なしのまま終了します）。今すぐ終えるなら「停止」とだけコメントしてください。\n\n期限: %s。期限を過ぎると本番反映は行わず終了し、ステージングの変更はそのまま残ります。\n\n",
		n, when)
	return body + CommentFacts{
		State:      "本番反映の承認待ち",
		NextActor:  "依頼者",
		Operation:  "「Go」で本番反映。反映しないなら何もしない（期限で終了）。今すぐ終えるなら「停止」",
		NextEvent:  "期限 " + when + "（期限切れで本番反映なしのまま終了）",
		Production: "未変更",
		AutoRetry:  "なし（人の判断待ち）",
		Marker:     GoReminderMarker(runID, n),
	}.render()
}

// LateWordMarker names the one answer a run gives to words that arrived
// after its wait had expired.
func LateWordMarker(runID string) string {
	return CommentMarker(string(RunCommentLateWord), runID)
}

// LateWordContent answers a Go or an answer posted after the wait it was
// for had expired. It must say two things and nothing more: nothing
// resumes, and how to continue — a late word is neither ignored nor acted
// on (acting on it would move production on evidence the wait was for).
func LateWordContent(runID, word string) string {
	body := fmt.Sprintf(
		"【期限切れで終了済み】この依頼は待ちの期限が切れて終了しているため、この「%s」では自動処理は再開しません。続けるには、同じ内容で新しく起票してください（拾い直し）。終了時点の状態（ステージングの変更や PR）はそのまま残っています。\n\n",
		word)
	return body + CommentFacts{
		State:      "終了済み（期限切れ）",
		NextActor:  "依頼者",
		Operation:  "続けるなら同じ内容で再度起票",
		NextEvent:  "なし（この依頼での自動処理は終了）",
		Production: "未変更",
		AutoRetry:  "なし",
		Marker:     LateWordMarker(runID),
	}.render()
}

// TerminalCommentMarkerPrefix is the leading part of the marker a terminal
// report carries (kind, run and code, before the report digest), so a
// terminal comment can be found without knowing the digest.
func TerminalCommentMarkerPrefix(runID, code string) string {
	return TerminalMarkerPrefix(runID) + code + ":"
}
