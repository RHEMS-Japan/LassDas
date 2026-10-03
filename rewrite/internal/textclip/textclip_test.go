package textclip

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayKeepsWholeGraphemesWithinTheExistingRuneBudget(t *testing.T) {
	for _, cluster := range []string{"🇯🇵", "👩‍👩‍👧‍👦", "👍🏽", "e\u0301", "\r\n", "각", "क्ष", "1️⃣"} {
		t.Run(cluster, func(t *testing.T) {
			for available := 1; available < utf8.RuneCountInString(cluster); available++ {
				prefix := strings.Repeat("x", 200-available)
				input := prefix + cluster + "remaining"
				if got := Clip(input, 200); got != prefix+"…" {
					t.Errorf("cut was hidden or split: %q", got)
				}
			}
			prefix := strings.Repeat("x", 200-utf8.RuneCountInString(cluster))
			if got := Clip(prefix+cluster, 200); got != prefix+cluster {
				t.Errorf("complete boundary cluster was lost or marked as cut: %q", got)
			}
			if got := Clip(prefix+cluster+"z", 200); got != prefix+cluster+"…" {
				t.Errorf("complete cluster before omitted suffix changed: %q", got)
			}
		})
	}
}

func TestDisplayBudgetsAndInvalidUTF8(t *testing.T) {
	for _, sample := range []struct {
		input string
		limit int
		want  string
	}{
		{"", 0, ""}, {"text", 0, "…"}, {"text", -1, "…"},
		{"a", 1, "a"}, {"ab", 1, "a…"}, {"日本語", 2, "日本…"},
		{"e" + strings.Repeat("\u0301", 10000), 200, "…"},
		{"x\xffz", 3, "x�z"}, {"x\xffz", 2, "x�…"},
		{"  retained  ", 20, "  retained  "},
	} {
		if got := Clip(sample.input, sample.limit); got != sample.want || !utf8.ValidString(got) {
			t.Errorf("display %q limit %d = %q (length %d), want %q", sample.input[:min(len(sample.input), 20)], sample.limit, got[:min(len(got), 80)], len(got), sample.want)
		}
	}
}
