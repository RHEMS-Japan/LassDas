package hook

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The implementer answered and its answer is on the ticket, so the fixed
// fields of the comment have to send the requester to it. The default line
// — an operator will look, the requester need do nothing — was written for
// endings nobody but an operator can act on, and it sent the one person who
// can act on the report away from it.
func TestAReturnedImplementationTellsTheRequesterTheMoveIsTheirs(t *testing.T) {
	comment := TerminalCommentContent(terminalTestRequest(TerminalImplementationReturned), strings.Repeat("f", 64))
	for _, expected := range []string{
		"次に行動する人: 起票者",
		"実装役の報告をご確認のうえ",
		"変更を加えずに理由を報告して作業を返しました",
		"本番の状態: 未変更",
	} {
		if !strings.Contains(comment, expected) {
			t.Fatalf("the comment lacks %q:\n%s", expected, comment)
		}
	}
	if strings.Contains(comment, "起票者の操作は不要です") {
		t.Fatalf("the comment tells the requester to do nothing about a report only they can act on:\n%s", comment)
	}
	if strings.Contains(comment, "次に行動する人: 運用担当者") {
		t.Fatalf("the comment hands the report to an operator:\n%s", comment)
	}
}

// The comment's fixed fields send the requester to the report below them,
// so the report has to be there. An agent writes one long paragraph as
// readily as it writes lines, and a record shortened for the comment used
// to end at the last line boundary in reach — the heading above the
// paragraph — leaving the heading, the note saying there was more, and
// nothing of what they introduced.
func TestAReturnedImplementationsCommentKeepsTheReportItPointsAt(t *testing.T) {
	const heading = "## 実装役の報告\n"
	paragraph := strings.Repeat("この依頼は、いまのままでは実現できません。", 600)
	report := terminalTestRequest(TerminalImplementationReturned)
	// The record as the run composed it: a short preamble on its own lines,
	// then the agent's words as one paragraph with no break of its own.
	record := "### 実装の経過 (1 周目で停止)\n- 実装役は、変更を加えずに理由を報告して作業を返しました。\n\n" + heading + paragraph + "\n"
	report.TrailText = ShortenTrailForComment(record, MaxTerminalTrailBytes)
	if err := ValidateTrailText(report.TrailText); err != nil {
		t.Fatalf("the record the run would send is invalid: %v", err)
	}
	comment := TerminalCommentContent(report, strings.Repeat("f", 64))
	if len(comment) > MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(comment), MaxTrackerCommentBytes)
	}

	at := strings.Index(comment, heading)
	if at < 0 {
		t.Fatalf("the report section is missing:\n%s", comment)
	}
	// A prefix of the agent's own words, not just the heading and a note.
	if body := comment[at+len(heading):]; !strings.HasPrefix(body, strings.Repeat("この依頼は、いまのままでは実現できません。", 20)) {
		t.Fatalf("the report the comment points at was cut away: %.160q", body)
	}
	if !strings.Contains(comment, TrailShortenedNote) {
		t.Fatalf("a shortened record does not say where the rest is:\n%s", comment)
	}
	// The footer still closes the comment: its final line is the marker the
	// exactly-once machinery anchors on, and a record that pushed it aside
	// would strand the run.
	lines := strings.Split(strings.TrimRight(comment, "\n"), "\n")
	if last := lines[len(lines)-1]; ExtractCommentMarker(comment) == "" || !strings.HasPrefix(last, "[") {
		t.Fatalf("the marker is not the last line: %q", last)
	}
	if !utf8.ValidString(comment) || strings.Contains(comment, "�") {
		t.Fatal("the cut landed inside a character")
	}
}

// The operator's status page renders every ending through this, and an
// ending with no case here shows the raw code beside "失敗で終了" — which
// is both unreadable and wrong, because nothing failed.
func TestAReturnedImplementationHasAnOperatorDescription(t *testing.T) {
	described := DescribeTerminalCode(string(TerminalImplementationReturned))
	if described == string(TerminalImplementationReturned) {
		t.Fatal("the code fell through to its own raw value")
	}
	if !strings.Contains(described, "実装役") || !strings.Contains(described, "(implementation_returned)") {
		t.Fatalf("description = %q", described)
	}
}
