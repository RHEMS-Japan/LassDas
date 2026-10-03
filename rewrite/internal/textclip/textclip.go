// Package textclip bounds display text without cutting a Unicode 17 extended
// grapheme cluster. It does not change the original record or validate input.
package textclip

import (
	"strings"
	"unicode/utf8"

	"github.com/clipperhouse/uax29/v2/graphemes"
)

// Clip returns complete graphemes within a code-point budget, adding an
// ellipsis when text is omitted. This is not a grapheme count: one cluster may
// contain arbitrarily many code points.
// Invalid UTF-8 is replaced only in the display, never in the caller's record.
func Clip(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	iterator := graphemes.FromString(text)
	end, count := 0, 0
	for iterator.Next() {
		cluster := iterator.Value()
		count += utf8.RuneCountInString(cluster)
		if count > limit {
			return text[:end] + "…"
		}
		end += len(cluster)
	}
	return text
}
