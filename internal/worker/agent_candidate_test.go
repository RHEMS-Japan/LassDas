package worker

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A tail cut through a multi-byte character used to leave broken lead bytes
// that the cleaning pass swelled into replacement runes past the byte
// budget - a 4,425-byte Japanese completion report failed its whole
// candidate as "content is invalid" on a live run. The rationale must stay
// within budget and valid for any transcript.
func TestAgentRationaleStaysWithinBudgetForMultibyteTails(t *testing.T) {
	for _, size := range []int{4425, 4097, 4098, 8000} {
		transcript := strings.Repeat("あ", (size/3)+2)
		transcript = transcript[:size]
		rationale := agentRationale(AgentRun{Transcript: transcript})
		if len(rationale) > 4096 || !utf8.ValidString(rationale) {
			t.Fatalf("size %d: rationale bytes=%d valid=%v", size, len(rationale), utf8.ValidString(rationale))
		}
		if err := validatePlainText(rationale, 4096, true); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
	}
}
