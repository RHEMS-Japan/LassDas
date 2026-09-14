package hook

import (
	"strings"
	"testing"
)

// A change the deployment was never going to pick up ends the delivery as
// done. The report must say the merge landed, that nothing is still coming,
// and that nobody has to look — the opposite of the absent-deploy report,
// which hands the ticket to an operator.
func TestTheReportForAChangeOutsideTheDeployScopeEndsTheDelivery(t *testing.T) {
	content := DeliverStagingContent("run-1", DeliverStagingReport{
		Verdict: "deploy_not_applicable",
		Detail:  "変更したファイル 2 件はすべて、設定されたステージング配布の対象範囲（src/, app/）の外です。配布の実行を待たずに完了します。",
	})
	for _, want := range []string{"配布対象外", "マージは済んでいます", "本番反映は行わず", "対象範囲（src/, app/）", "次に行動する人: なし"} {
		if !strings.Contains(content, want) {
			t.Errorf("the staging report lacks %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"運用担当者", "失敗", "確認できませんでした", "「Go」"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the staging report says %q, which is not what happened:\n%s", forbidden, content)
		}
	}
	release := DeliverReleaseContent("run-1", DeliverReleaseReport{Verdict: "deploy_not_applicable"})
	for _, want := range []string{"配布対象外", "本番ブランチへ反映済み", "次に行動する人: なし"} {
		if !strings.Contains(release, want) {
			t.Errorf("the release report lacks %q:\n%s", want, release)
		}
	}
	if strings.Contains(release, "運用担当者") {
		t.Errorf("the release report sends an operator looking:\n%s", release)
	}
}
