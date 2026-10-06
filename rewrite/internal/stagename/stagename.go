// Package stagename holds the Japanese names of the stages of the shipped
// runs. Two places show a stage to the person who filed the request, the
// status page and the runtime's own comments on the tracker, and they say the
// same stage by the same name. A stage an operator named otherwise is not
// here and keeps the name they gave it.
package stagename

var japanese = map[string]string{
	"elicit": "要件確定", "implement": "実装", "work": "作業", "verify": "検証", "review": "レビュー", "confirm_change": "納品前の確認", "deliver": "納品",
	"verify_merged": "マージ後検証", "draft_report": "報告", "report": "報告", "post_report": "報告の投稿", "review_report": "報告のレビュー",
	"confirm_report": "報告の照合", "ask_requester": "依頼者への質問", "stop_report": "停止の報告", "done": "完了", "other": "その他", "router": "本体の判断",
}

// Japanese is the stage's Japanese name, and whether it has one at all.
func Japanese(name string) (string, bool) {
	label, known := japanese[name]
	return label, known
}
