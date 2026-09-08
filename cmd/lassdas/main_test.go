package main

import (
	"bytes"
	"context"
	"strings"
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
