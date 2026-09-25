package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeResourceRecord(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resources.jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// What a run created outside the repository reaches the requester through
// the trail, which is the pull request body and the ticket's closing
// comment. Nothing else would ever mention it: it is not in the diff, and
// it stays there after the delivery ends.
func TestTheTrailNamesWhatTheRunCreated(t *testing.T) {
	config, request, source, candidate, reviews := nonconvergedFixture(t)
	decision, err := DecideStage(candidate, reviews, source, request, config, nil)
	if err != nil {
		t.Fatal(err)
	}
	stages := []trailStage{{
		Stage: 1, Candidate: candidate, Reviews: reviews, Decision: decision,
		Source: source, Request: request,
	}}
	created := []CreatedResource{
		{Kind: "sqs", Identifier: "lassdas-orders-intake", Provider: "aws", Stage: "implement", CreatedAt: time.Date(2026, 9, 25, 14, 46, 0, 0, time.UTC)},
		{Kind: "rds", Identifier: "lassdas-orders-db", Provider: "aws", Stage: "implement", CreatedAt: time.Date(2026, 9, 25, 14, 50, 0, 0, time.UTC)},
	}
	trail := ComposeTrailWithResources(stages, nil, true, "", created)
	for _, expected := range []string{
		"この依頼で作った資源",
		"sqs: lassdas-orders-intake",
		"rds: lassdas-orders-db",
		"(aws)",
		"implement の工程",
		"2026-09-25 14:46 UTC",
	} {
		if !strings.Contains(trail, expected) {
			t.Fatalf("the trail lacks %q:\n%s", expected, trail)
		}
	}
	// A run that created nothing keeps the record it always had.
	if plain := ComposeTrailWithResources(stages, nil, true, "", nil); strings.Contains(plain, "作った資源") {
		t.Fatalf("a run that created nothing grew a section:\n%s", plain)
	}
}

// A card that provisioned something and then died has left it behind, so
// the report of that run must not say the environments were untouched.
func TestAStoppedRunStillNamesWhatItCreated(t *testing.T) {
	round := UnsealedRound{Round: 2, Report: "接続できませんでした。"}
	created := []CreatedResource{{Kind: "s3", Identifier: "lassdas-orders-archive", Stage: "implement", CreatedAt: time.Now().UTC()}}
	stopped := ComposeUnsealedTrailWithResources(round, "実装", created)
	if !strings.Contains(stopped, "lassdas-orders-archive") {
		t.Fatalf("the stopped run's report lost what it created:\n%s", stopped)
	}
	if strings.Contains(stopped, "対象リポジトリと本番環境は変更していません") {
		t.Fatalf("the stopped run's report claims it changed nothing:\n%s", stopped)
	}
	if untouched := ComposeUnsealedTrail(round, "実装"); !strings.Contains(untouched, "対象リポジトリと本番環境は変更していません") {
		t.Fatalf("a run that created nothing lost the sentence that says so:\n%s", untouched)
	}
}

// The record is read back from the run directory. A line that cannot be
// read is skipped rather than losing the rest: the list exists so that what
// was made can still be found.
func TestLoadCreatedResourcesSkipsWhatItCannotRead(t *testing.T) {
	path := writeResourceRecord(t, "{\"kind\":\"sqs\",\"identifier\":\"one\"}\nnot json\n{\"kind\":\"s3\"}\n\n{\"kind\":\"s3\",\"identifier\":\"two\"}\n")
	created := LoadCreatedResources(path)
	if len(created) != 2 || created[1].Identifier != "two" {
		t.Fatalf("LoadCreatedResources() = %+v", created)
	}
	if got := LoadCreatedResources(filepath.Join(t.TempDir(), "absent.jsonl")); got != nil {
		t.Fatalf("a missing record read as %+v", got)
	}
	if got := LoadCreatedResources(""); got != nil {
		t.Fatalf("an unnamed record read as %+v", got)
	}
}

// A declaration is a model's own words, and both places it is shown render
// markup. An identifier written to look like the end of a link would come
// out as a link somebody might follow, believing the engine put it there.
func TestADeclarationCannotRenderAsMarkup(t *testing.T) {
	created := []CreatedResource{{
		Kind:       "sqs",
		Identifier: "x](https://elsewhere.invalid) `whoami`",
		Provider:   "<b>aws</b>",
		Stage:      "implement",
		CreatedAt:  time.Date(2026, 9, 25, 14, 46, 0, 0, time.UTC),
	}}
	rendered := composeCreatedResources(created)
	for _, construct := range []string{"](https://", "`whoami`", "<b>"} {
		if strings.Contains(rendered, construct) {
			t.Fatalf("a declaration rendered as markup (%q):\n%s", construct, rendered)
		}
	}
	// And the text is still there to read: escaped, not thrown away. What
	// is in the field is how the resource is found again.
	if !strings.Contains(rendered, "elsewhere.invalid") || !strings.Contains(rendered, "whoami") {
		t.Fatalf("the declaration was thrown away instead of escaped:\n%s", rendered)
	}
}

// An ordinary name comes out exactly as it went in. Escaping a hyphen or a
// full stop would put a backslash in every report, wherever the text is
// read as plain text.
func TestAnOrdinaryResourceNameIsUnchanged(t *testing.T) {
	created := []CreatedResource{{
		Kind:       "sqs",
		Identifier: "arn:aws:sqs:ap-northeast-1:123456789012/lassdas-orders-intake.fifo",
		Provider:   "aws",
		Stage:      "implement",
		CreatedAt:  time.Date(2026, 9, 25, 14, 46, 0, 0, time.UTC),
	}}
	rendered := composeCreatedResources(created)
	if !strings.Contains(rendered, "arn:aws:sqs:ap-northeast-1:123456789012/lassdas-orders-intake.fifo") {
		t.Fatalf("an ordinary name was altered:\n%s", rendered)
	}
	if strings.Contains(rendered, `\\`) {
		t.Fatalf("a backslash reached an ordinary name:\n%s", rendered)
	}
}

// The list says whose word it is. The engine has no standing to ask a
// provider about an account it reaches only through a credential the
// destination handed over, so it cannot check that any of this exists.
func TestTheListSaysItIsADeclarationAndNotACheck(t *testing.T) {
	rendered := composeCreatedResources([]CreatedResource{{Kind: "sqs", Identifier: "one", Stage: "implement"}})
	if !strings.Contains(rendered, "申告") {
		t.Fatalf("the list reads as something the engine verified:\n%s", rendered)
	}
}
