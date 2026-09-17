package main

import (
	"bytes"
	"context"
	"strings"
	"syscall"
	"testing"
)

func TestHelpAndInvalidArgumentsHaveNoRuntimeSideEffects(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"init", "--help"}, {"run", "status", "--help"}} {
		var out bytes.Buffer
		if err := run(context.Background(), args, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "lassdas init") || !strings.Contains(out.String(), "smoke") {
			t.Fatal("help does not explain startup and resume")
		}
	}
	for _, args := range [][]string{{"unknown"}, {"run"}, {"run", "delete"}, {"run", "stop", "--unknown"}, {"init", "unexpected"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatalf("bad args accepted: %v", args)
		}
	}
}

// Progress output goes to whoever is watching, and nobody may be: piping
// setup into `head` or a pager closed the reader partway through, the work
// stopped there, and the shell reported the pager's success (reported live
// 2026-09-17).
func TestProgressOutputNeverFailsTheWork(t *testing.T) {
	writer := quietWriter{brokenWriter{}}
	if n, err := writer.Write([]byte("段階: prepare\n")); err != nil || n != len("段階: prepare\n") {
		t.Fatalf("a closed reader reached the caller: n=%d err=%v", n, err)
	}
	var kept strings.Builder
	watched := quietWriter{&kept}
	if _, err := watched.Write([]byte("段階: runtime\n")); err != nil || kept.String() != "段階: runtime\n" {
		t.Fatalf("the line did not reach a reader that is there: %q %v", kept.String(), err)
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, syscall.EPIPE }
