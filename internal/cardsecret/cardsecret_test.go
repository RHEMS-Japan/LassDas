package cardsecret

import (
	"strings"
	"testing"
)

// A credentials file reaches a log one line at a time: a tool prints the
// profile it could not use, a shell echoes the line it read. A record
// holding one of those lines has published it, so every line is a secret in
// its own right.
func TestEveryLineOfAMultiLineCredentialIsASecret(t *testing.T) {
	Forget()
	t.Cleanup(Forget)
	file := "[dev]\naws_access_key_id = AKIAEXAMPLEEXAMPLE\naws_secret_access_key = wJalrXUtnFEMIexampleKEY\n"
	Register([]string{"AWS_SHARED_CREDENTIALS_FILE"}, []string{file})

	said := "profile load failed: aws_secret_access_key = wJalrXUtnFEMIexampleKEY"
	if got := Redact(said); strings.Contains(got, "wJalrXUtnFEMIexampleKEY") {
		t.Fatalf("one line of the file survived: %q", got)
	}
	// A line shorter than the bound is left alone: a section header is a
	// word a log is full of, and a record where every third word reads
	// [secret] says nothing about why a card failed.
	if got := Redact("could not read [dev]"); !strings.Contains(got, "[dev]") {
		t.Fatalf("a line too short to be a secret was taken out: %q", got)
	}
	whole := Redact("the file is " + file)
	if strings.Contains(whole, "AKIAEXAMPLEEXAMPLE") {
		t.Fatalf("the whole file survived: %q", whole)
	}
}

// A value the text begins in the middle of is what a tail buffer leaves
// when a single line is longer than the buffer: what survives is the end of
// a secret, and no whole-value replacement finds it.
func TestAValueTheTextBeginsInTheMiddleOfIsTakenOut(t *testing.T) {
	Forget()
	t.Cleanup(Forget)
	value := "postgres://warehouse.invalid/orders?password=hunter2hunter2"
	Register([]string{"DATABASE_URL"}, []string{value})

	partial := value[7:] + " could not be reached"
	got := Redact(partial)
	if strings.Contains(got, "hunter2hunter2") {
		t.Fatalf("the end of the value survived: %q", got)
	}
	if !strings.Contains(got, "could not be reached") {
		t.Fatalf("the sentence around it was lost: %q", got)
	}
}

// A card handed nothing registers nothing and changes no text.
func TestACardWithNoCredentialsChangesNothing(t *testing.T) {
	Forget()
	t.Cleanup(Forget)
	said := "psql: could not connect to the server"
	if got := Redact(said); got != said {
		t.Fatalf("text was changed with nothing registered: %q", got)
	}
	if len(Names()) != 0 || len(Literals()) != 0 {
		t.Fatalf("something was registered: %v / %v", Names(), Literals())
	}
}

// The names travel to the processes a card starts; the values travel as the
// variables themselves, which is what a started process already has.
func TestTheNameListIsWhatAStartedProcessCannotWorkOutForItself(t *testing.T) {
	Forget()
	t.Cleanup(Forget)
	t.Setenv("DATABASE_URL", "postgres://warehouse.invalid/orders")
	t.Setenv("API_TOKEN", "a-long-enough-token-value")
	t.Setenv(NamesEnv, "DATABASE_URL:API_TOKEN")
	FromEnvironment()

	names := Names()
	if len(names) != 2 || names[0] != "DATABASE_URL" {
		t.Fatalf("the names read as %v", names)
	}
	if got := Redact("psql: postgres://warehouse.invalid/orders refused"); strings.Contains(got, "warehouse.invalid") {
		t.Fatalf("a value named by the list was kept: %q", got)
	}
}

// Longest first, so a value that contains a shorter one is taken out whole
// rather than in pieces.
func TestTheLongestValueIsTakenOutFirst(t *testing.T) {
	Forget()
	t.Cleanup(Forget)
	Register([]string{"SHORT", "LONG"}, []string{"orders-intake", "orders-intake-queue-name"})
	got := Redact("created orders-intake-queue-name")
	if got != "created "+Redacted {
		t.Fatalf("the shorter value was taken out first: %q", got)
	}
}
