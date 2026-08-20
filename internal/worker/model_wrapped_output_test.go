package worker

import (
	"encoding/json"
	"testing"
)

// The direct model paths tolerate prose/fence wrapping by peeling the last
// JSON object before the unchanged strict decode — the same tolerance the
// agent-review path always had. These exercise the extractor composition
// the fallback relies on.
func TestLastJSONObjectPeelsWrappedReview(t *testing.T) {
	inner := `{"verdict":"revise","findings":[{"code":"x-y","path":"a.ts","line":1,"message":"m"}]}`
	for name, wrapped := range map[string]string{
		"prose":   "レビューしました。以下が判定です。\n" + inner + "\n以上です。",
		"fence":   "```json\n" + inner + "\n```",
		"both":    "結果:\n```\n" + inner + "\n```\nご確認ください。",
		"asIsish": inner,
	} {
		block, err := lastJSONObject(wrapped)
		if err != nil {
			t.Fatalf("%s: lastJSONObject() error = %v", name, err)
		}
		output, err := DecodeModelReviewOutput([]byte(block))
		if err != nil {
			t.Fatalf("%s: DecodeModelReviewOutput() error = %v", name, err)
		}
		if output.Verdict != "revise" || len(output.Findings) != 1 {
			t.Fatalf("%s: decoded %+v", name, output)
		}
	}
}

func TestLastJSONObjectStillRefusesGarbage(t *testing.T) {
	for name, garbage := range map[string]string{
		"no json":    "承認します。問題ありません。",
		"unbalanced": "{\"verdict\":\"pass\"",
	} {
		block, err := lastJSONObject(garbage)
		if err == nil {
			var probe map[string]any
			if json.Unmarshal([]byte(block), &probe) == nil {
				if _, decodeErr := DecodeModelReviewOutput([]byte(block)); decodeErr == nil {
					t.Fatalf("%s: garbage was accepted as a review", name)
				}
			}
		}
	}
}
