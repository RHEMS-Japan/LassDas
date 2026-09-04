package hook

import (
	"strings"
	"testing"
)

func TestDeliverResolvedContentCarriesTheMarkerAndNamesProduction(t *testing.T) {
	staging := DeliverResolvedContent("RUN-1", DeliverResolvedReport{Phase: "staging", Verdict: "deploy_failed"})
	if ExtractCommentMarker(staging) != CommentMarker("resolved", "RUN-1") {
		t.Fatalf("staging content marker = %q", ExtractCommentMarker(staging))
	}
	if !strings.Contains(staging, "本番への反映は自動では行わず") || !strings.Contains(staging, "本番の状態: 未変更") {
		t.Fatalf("staging content does not say what happens to production:\n%s", staging)
	}
	release := DeliverResolvedContent("RUN-1", DeliverResolvedReport{Phase: "release", Verdict: "merge_unverified"})
	if ExtractCommentMarker(release) != CommentMarker("resolved", "RUN-1") {
		t.Fatalf("release content marker = %q", ExtractCommentMarker(release))
	}
	if !strings.Contains(release, "本番反映の状態は運用担当者が確認しました") {
		t.Fatalf("release content:\n%s", release)
	}
}
