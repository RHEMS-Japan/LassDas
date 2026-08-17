package hook

import (
	"fmt"
	"regexp"
	"strings"
)

// Every automated Backlog comment ends with the same seven-item block
// (README「Backlog 上の表示と通知」): state, who acts next, the concrete
// operation, the next notification or deadline, the production state, whether
// the automation retries, and a machine marker. The block is deterministic so
// a lost POST is repaired by searching for the marker, and duplicate postings
// are detectable from the marker alone.

const commentMarkerPrefix = "ticket-automation:v1"

var commentMarkerPattern = regexp.MustCompile(`^\[ticket-automation:v1:[a-z-]{1,32}:[A-Za-z0-9_-]{1,128}(?::[A-Za-z0-9_.-]{1,64})*\]$`)

// CommentMarker renders the machine identifier for one automated comment:
// kind, run, and the kind-specific qualifiers (question revision, notification
// number, digest prefix, ...).
func CommentMarker(kind, runID string, qualifiers ...string) string {
	parts := append([]string{commentMarkerPrefix, kind, runID}, qualifiers...)
	return "[" + strings.Join(parts, ":") + "]"
}

// ExtractCommentMarker returns the machine marker of a comment, which is
// always its final line. Anchoring to the end is what keeps the identifier
// trustworthy: question text comes from a model and comment bodies come from
// the requester, so either could contain a marker-shaped string, but neither
// can put it after our footer.
func ExtractCommentMarker(body string) string {
	trimmed := strings.TrimRight(body, " \t\r\n")
	index := strings.LastIndex(trimmed, "\n")
	last := trimmed[index+1:]
	if commentMarkerPattern.FindString(last) != last {
		return ""
	}
	return last
}

// CommentFacts is the seven-item footer of one automated comment. Every field
// is user-facing prose except Marker.
type CommentFacts struct {
	State      string
	NextActor  string
	Operation  string
	NextEvent  string
	Production string
	AutoRetry  string
	Marker     string
}

func (f CommentFacts) render() string {
	var builder strings.Builder
	builder.WriteString("\n---\n")
	fmt.Fprintf(&builder, "状態: %s\n", f.State)
	fmt.Fprintf(&builder, "次に行動する人: %s\n", f.NextActor)
	fmt.Fprintf(&builder, "操作: %s\n", f.Operation)
	fmt.Fprintf(&builder, "次回通知・期限: %s\n", f.NextEvent)
	fmt.Fprintf(&builder, "本番の状態: %s\n", f.Production)
	fmt.Fprintf(&builder, "自動再試行: %s\n", f.AutoRetry)
	builder.WriteString(f.Marker + "\n")
	return builder.String()
}

// ValidateCommentContract checks that a rendered comment carries every
// contract item and exactly the expected marker. It backs the table tests
// that pin all automated comment kinds to the README contract.
func ValidateCommentContract(body, marker string) error {
	for _, item := range []string{
		"状態: ", "次に行動する人: ", "操作: ", "次回通知・期限: ", "本番の状態: ", "自動再試行: ",
	} {
		if !strings.Contains(body, item) {
			return fmt.Errorf("comment lacks the contract item %q", strings.TrimSuffix(item, ": "))
		}
	}
	if marker == "" || ExtractCommentMarker(body) != marker {
		return fmt.Errorf("comment marker mismatch: found %q", ExtractCommentMarker(body))
	}
	return nil
}
