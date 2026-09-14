package hook

import (
	"strings"
	"testing"
	"time"
)

// A queued ticket under the operator's pause is told three things: the
// request was received, nothing is wrong with it, and it starts when intake
// resumes. It must not read as a failure and must carry the marker the
// attendant looks for so the notice is posted once.
func TestTheIntakePausedNoticeSaysReceivedNotFailed(t *testing.T) {
	since := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)
	content := IntakePausedContent("TKT-3", since)
	for _, want := range []string{"【受付停止中】", "受け付けました", "再開後に", "実行中の依頼はそのまま進みます", "停止: 2026-09-14 09:00", "次に行動する人: 運用担当者"} {
		if !strings.Contains(content, want) {
			t.Errorf("the notice lacks %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"失敗", "エラー", "確認済み"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the notice says %q, which is not what happened:\n%s", forbidden, content)
		}
	}
	if ExtractCommentMarker(content) != CommentMarker(string(RunCommentIntakePaused), "TKT-3") {
		t.Fatalf("the notice does not carry its marker: %q", ExtractCommentMarker(content))
	}
}
