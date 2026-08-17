package worker

import (
	"strings"
	"testing"
)

// Two different diseases must carry two different names, or a dead run says
// nothing about which one killed it.
func TestDecodeModelDeriveOutputNamesEachRefusal(t *testing.T) {
	if _, err := DecodeModelDeriveOutput([]byte("```json\n{}\n```")); err == nil || !strings.Contains(err.Error(), "not the demanded strict json") {
		t.Fatalf("markdown-fenced response: %v", err)
	}
	if _, err := DecodeModelDeriveOutput([]byte(`{"files": ["a.ts"], "rationale": "r", "extra": 1}`)); err == nil || !strings.Contains(err.Error(), "not the demanded strict json") {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := DecodeModelDeriveOutput([]byte(`{"files": [], "rationale": "r"}`)); err == nil || !strings.Contains(err.Error(), "names no files") {
		t.Fatalf("empty file list: %v", err)
	}
	output, err := DecodeModelDeriveOutput([]byte(`{"files": ["ui/dashboard/src/lib/currency.ts"], "rationale": "r"}`))
	if err != nil || len(output.Files) != 1 {
		t.Fatalf("valid output refused: %+v, %v", output, err)
	}
}
