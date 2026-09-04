package hook

import (
	"strings"
	"testing"
)

// A page the browser never reached is reported as unjudged — never as a
// failed screen check — and the footer names who has to act: the operator
// when the login did not land, the requester when the page sent the
// browser elsewhere.
func TestObserveBlockedReportsNameWhoActs(t *testing.T) {
	signIn := DeliverStagingContent("RUN-1", DeliverStagingReport{
		Verdict: "observe_blocked", Block: "sign_in", TargetURL: "https://stg.example.invalid/console/new",
		ExpectedText: "Connect", Detail: "確認用のログイン状態が切れているか、取り消されています。",
	})
	for _, needle := range []string{"【ステージング確認ができず】", "判定できませんでした", "次に行動する人: 運用担当者", "確認用のログインをやり直し", "本番の状態: 未変更", "出ているべき表示: 「Connect」"} {
		if !strings.Contains(signIn, needle) {
			t.Fatalf("sign-in block lacks %q:\n%s", needle, signIn)
		}
	}
	if strings.Contains(signIn, "不合格") {
		t.Fatalf("an unjudged screen must not read as a failed check:\n%s", signIn)
	}
	if ExtractCommentMarker(signIn) != CommentMarker("stg-report", "RUN-1") {
		t.Fatalf("marker = %q", ExtractCommentMarker(signIn))
	}

	redirect := DeliverStagingContent("RUN-1", DeliverStagingReport{Verdict: "observe_blocked", Block: "redirect", Detail: "転送先: https://portal.example.invalid/"})
	for _, needle := range []string{"【ステージング確認ができず】", "次に行動する人: 依頼者", "転送されない画面を確認先に指定して起票し直してください", "「確認済み」"} {
		if !strings.Contains(redirect, needle) {
			t.Fatalf("redirect block lacks %q:\n%s", needle, redirect)
		}
	}

	release := DeliverReleaseContent("RUN-1", DeliverReleaseReport{Verdict: "observe_blocked", Block: "sign_in"})
	for _, needle := range []string{"【本番反映済み・画面確認ができず】", "次に行動する人: 運用担当者", "確認用のログインをやり直し", "本番の状態: 反映済み（画面の機械確認は判定不能）"} {
		if !strings.Contains(release, needle) {
			t.Fatalf("release block lacks %q:\n%s", needle, release)
		}
	}
	if strings.Contains(release, "不合格") {
		t.Fatalf("an unjudged production screen must not read as failed:\n%s", release)
	}
}

func TestSessionHoldContentCarriesTheMarkerAndNamesTheDestination(t *testing.T) {
	content := SessionHoldContent("RUN-3", []string{"https://stg.example.invalid"})
	if ExtractCommentMarker(content) != CommentMarker("session-hold", "RUN-3") {
		t.Fatalf("marker = %q", ExtractCommentMarker(content))
	}
	for _, needle := range []string{"【確認用のログイン状態が切れているため開始できません】", "https://stg.example.invalid", "人の操作なしで自動的に開始します", "次に行動する人: 運用担当者", "自動再試行: あり（10 分ごと）", "本番の状態: 未変更"} {
		if !strings.Contains(content, needle) {
			t.Fatalf("session hold lacks %q:\n%s", needle, content)
		}
	}
}
