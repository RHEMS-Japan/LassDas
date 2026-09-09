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
		Detail:  "設定されたステージングのデプロイ処理が、このマージに対して実行を 1 つも作りませんでした。",
	})
	for _, want := range []string{
		"デプロイの実行が作られませんでした",
		"実行を 1 つも作りませんでした",
		"画面での確認は行っていません",
		"本番反映も行いません",
		"運用担当者",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("the report lacks %q:\n%s", want, content)
		}
	}
	// The claims an earlier wording made that were never measured: only one
	// configured deployment was watched, and whether the branch still
	// carries the change was not re-read at the end.
	for _, forbidden := range []string{"確認できませんでした", "「Go」", "このリポジトリの自動デプロイ", "ブランチに入りました"} {
		if strings.Contains(content, forbidden) {
			t.Errorf("the report says %q, which is not what happened:\n%s", forbidden, content)
		}
	}
}
