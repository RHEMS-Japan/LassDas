package hook

import (
	"strings"
	"testing"
)

// What a requester is told when the merge landed and nothing deployed it.
// The two things it must not say are that something failed and that
// something is still coming: neither is true, and both send someone looking.
func TestTheReportForADeployThatNeverStartedSaysBothHalves(t *testing.T) {
	content := DeliverStagingContent("run-1", DeliverStagingReport{
		Verdict: "deploy_absent",
		Detail:  "変更はステージングのブランチに入りましたが、このリポジトリの自動デプロイは今回のマージでは 1 度も起動しませんでした。",
	})
	for _, want := range []string{
		"ステージングのブランチに反映済み",
		"1 度も起動しませんでした",
		"画面での確認は行っていません",
		"本番反映も行いません",
		"運用担当者",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the report lacks %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{"確認できませんでした", "「Go」"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the report says %q, which is not what happened:\n%s", forbidden, content)
		}
	}
}
