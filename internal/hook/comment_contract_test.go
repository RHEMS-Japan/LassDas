package hook

import (
	"strings"
	"testing"
)

func TestTerminalMarkerPrefixAndCode(t *testing.T) {
	digest := strings.Repeat("a", 64)
	marker := CommentMarker("terminal", "TICKET-7", string(TerminalSuccess), digest)
	prefix := TerminalMarkerPrefix("TICKET-7")
	if !strings.HasPrefix(marker, prefix) || strings.HasPrefix(CommentMarker("terminal", "TICKET-70", "success", digest), prefix) {
		t.Fatalf("prefix %q must match %q and not TICKET-70", prefix, marker)
	}
	if strings.HasPrefix(CommentMarker("ack", "TICKET-7"), prefix) {
		t.Fatal("an ack marker must not read as a terminal report")
	}
	if got := TerminalCodeFromMarker(marker); got != "success" {
		t.Fatalf("code = %q", got)
	}
	if got := TerminalCodeFromMarker(CommentMarker("ack", "TICKET-7")); got != "" {
		t.Fatalf("ack marker yielded code %q", got)
	}
	if got := TerminalCodeFromMarker("not a marker"); got != "" {
		t.Fatalf("junk yielded code %q", got)
	}
}
