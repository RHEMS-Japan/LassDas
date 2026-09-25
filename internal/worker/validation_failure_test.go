package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// What a step printed has been cut at a byte boundary twice before it reaches
// here, so its first character is very often half of one. A record built from
// it must carry whole characters: it is rendered into a prompt and digested,
// and a broken sequence would travel into both.
func TestTheBoundedOutputKeepsWholeCharactersAndTheEnd(t *testing.T) {
	// Three-byte characters, so no cut at the budget lands on a boundary.
	whole := strings.Repeat("あ", 4000) + "終"
	// Cut mid-character at the front, the way the tails upstream cut.
	bounded := boundedValidationOutput(whole[1:])
	if !utf8.ValidString(bounded) {
		t.Fatalf("the bounded output is not valid UTF-8: %q", bounded)
	}
	if len(bounded) > MaxValidationOutputBytes {
		t.Fatalf("bounded output = %d bytes, budget is %d", len(bounded), MaxValidationOutputBytes)
	}
	// The end is what a build or test runner puts its verdict in, so the end
	// is what survives.
	if !strings.HasSuffix(bounded, "あ終") {
		t.Fatalf("the end of the output was not kept: %q", bounded[max(0, len(bounded)-32):])
	}
	// Anything that already fits is untouched, and the bytes that carry no
	// text at all go.
	if got := boundedValidationOutput("FAIL\x00 one test\n"); got != "FAIL one test\n" {
		t.Fatalf("a short output was not passed through cleanly: %q", got)
	}
}

// A worst-case record still reads back. The output is bounded in bytes and
// the record is read back with a bound of its own in another process, so an
// output made entirely of the characters JSON escapes most must still fit
// inside that bound — a record that cannot be read is a round that loses the
// one thing it is being repeated for.
func TestAWorstCaseRecordStaysInsideTheReadBound(t *testing.T) {
	record := NewValidationFailure(2, "run-validation", strings.Repeat("\x01", 8*MaxValidationOutputBytes))
	record.DeliveryID = "delivery_" + strings.Repeat("ab", 16)
	record.InputSHA256 = strings.Repeat("1", 64)
	record.ConfigSHA256 = strings.Repeat("2", 64)
	record.ToolSHA = strings.Repeat("3", 40)
	if err := record.Seal(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(encoded)) > MaxValidationFailureJSONBytes {
		t.Fatalf("a worst-case record is %d bytes, read bound is %d", len(encoded), MaxValidationFailureJSONBytes)
	}
	// And it leaves an instruction room for the change itself.
	if MaxValidationOutputBytes >= MaxAgentPromptBytes/2 {
		t.Fatalf("the output budget %d leaves no room in a %d-byte prompt", MaxValidationOutputBytes, MaxAgentPromptBytes)
	}
}

// The digest is the material a later reader compares to notice a run
// repeating itself, so the same failure printed twice has to digest the same
// even when the runner's spacing does not repeat exactly — and two different
// failures must not.
func TestTheSameFailurePrintedTwiceDigestsTheSame(t *testing.T) {
	first := NewValidationFailure(2, "run-validation", "--- FAIL: TestLabel (0.00s)\n    label_test.go:21: want 'new', got 'old'\n")
	// The same text as a runner on another invocation might pad and end it.
	second := NewValidationFailure(3, "run-validation", "\r\n--- FAIL: TestLabel (0.00s)   \r\n    label_test.go:21: want 'new', got 'old'\t\r\n\r\n")
	if first.OutputSHA256 != second.OutputSHA256 {
		t.Fatalf("the same failure digested differently:\n%s\n%s", first.OutputSHA256, second.OutputSHA256)
	}
	// The round is not in the digest: it is what the two being compared
	// differ by.
	if first.Round == second.Round {
		t.Fatal("the fixture compares one round with itself")
	}
	third := NewValidationFailure(4, "run-validation", "--- FAIL: TestOther (0.00s)\n")
	if third.OutputSHA256 == first.OutputSHA256 {
		t.Fatal("two different failures digested the same")
	}
}

// A record is read back only when it is one of ours and intact. The round
// after this one is about to be told what is in it, so a truncated write, a
// record from another round, and one whose output was edited after sealing
// are all refused rather than repaired.
func TestARecordThatDoesNotBindIsRefused(t *testing.T) {
	directory := t.TempDir()
	seal := func(t *testing.T, name string, mutate func(*ValidationFailure)) string {
		t.Helper()
		record := NewValidationFailure(1, "run-validation", "--- FAIL\n")
		if err := record.Seal(); err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(&record)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	intact := seal(t, "intact.json", nil)
	record, err := ReadValidationFailureFile(intact)
	if err != nil || record.Step != "run-validation" || record.Round != 1 {
		t.Fatalf("an intact record was refused: %+v, %v", record, err)
	}
	if record.Bound(2) {
		t.Fatal("a record for round 1 answered for round 2")
	}
	for name, mutate := range map[string]func(*ValidationFailure){
		"edited.json":     func(v *ValidationFailure) { v.Output = "--- PASS\n" },
		"reround.json":    func(v *ValidationFailure) { v.Round = 9 },
		"unsealed.json":   func(v *ValidationFailure) { v.RecordSHA256 = "" },
		"unnamed.json":    func(v *ValidationFailure) { v.Step = "" },
		"fromlater.json":  func(v *ValidationFailure) { v.SchemaVersion = ValidationFailureSchemaVersion + 1 },
		"redigested.json": func(v *ValidationFailure) { v.OutputSHA256 = validationOutputDigest("--- PASS\n") },
	} {
		if _, err := ReadValidationFailureFile(seal(t, name, mutate)); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	if _, err := ReadValidationFailureFile(filepath.Join(directory, "absent.json")); err == nil {
		t.Fatal("a missing record was accepted")
	}
}
