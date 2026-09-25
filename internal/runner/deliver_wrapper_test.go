package runner

import (
	"os"
	"strings"
	"testing"
)

// TestEveryDeliveryVerbGoesThroughTheWrapper holds the delivery verbs to the
// one call that reads the context after the controller returns. The verbs'
// own tests reach one verb, so a call site put back on the bare controller
// would pass them all and report a killed verb as a red gate again; the
// source is the only place every call site can be seen at once.
func TestEveryDeliveryVerbGoesThroughTheWrapper(t *testing.T) {
	raw, err := os.ReadFile("deliver.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if n := strings.Count(src, "p.controller("); n != 1 {
		t.Fatalf("deliver.go calls p.controller( %d times; only deliverVerb may, every verb goes through p.deliverVerb(", n)
	}
	start := strings.Index(src, "func (p *Pipeline) deliverVerb(")
	if start < 0 {
		t.Fatal("deliverVerb is not in deliver.go")
	}
	end := strings.Index(src[start+1:], "\nfunc ")
	if end < 0 {
		end = len(src)
	} else {
		end += start + 1
	}
	if call := strings.Index(src, "p.controller("); call < start || call > end {
		t.Fatalf("the one p.controller( call is outside deliverVerb, so a verb is bypassing the wrapper")
	}
	if n := strings.Count(src, "p.deliverVerb("); n < 9 {
		t.Fatalf("only %d verbs go through the wrapper; the delivery has nine", n)
	}
}
