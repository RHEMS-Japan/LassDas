package livelog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openAt(t *testing.T, path string) *Sink {
	t.Helper()
	t.Setenv(PathEnv, path)
	sink := Open()
	if sink == nil {
		t.Fatal("Open() returned no sink for an absolute path")
	}
	return sink
}

func TestOnlyWholeLinesReachTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "implement.log")
	sink := openAt(t, path)
	if _, err := sink.Write([]byte("実装を開始し")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a partial line was written")
	}
	if _, err := sink.Write([]byte("ます\n次の行")); err != nil {
		t.Fatal(err)
	}
	if body := read(t, path); body != "実装を開始します\n" {
		t.Fatalf("file = %q, want only the finished line", body)
	}
	sink.Close()
	if body := read(t, path); body != "実装を開始します\n次の行\n" {
		t.Fatalf("file = %q, want the unfinished line flushed on close", body)
	}
}

// A secret in the output never reaches the file, and it cannot slip through
// by arriving in two chunks: masking sees whole lines only.
func TestSecretsAreMaskedAcrossChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "review.log")
	sink := openAt(t, path)
	_, _ = sink.Write([]byte("TARGET_GITHUB_TOKEN=ghp_abcdefghij"))
	_, _ = sink.Write([]byte("klmnopqrstuvwxyz0123456789\n"))
	body := read(t, path)
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("the token reached the file: %q", body)
	}
	if strings.TrimSpace(body) == "" {
		t.Fatal("the line vanished entirely")
	}
}

func TestBoundKeepsTheBeginningAndSaysItWasCut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "loop.log")
	sink := openAt(t, path)
	line := strings.Repeat("a", 1024) + "\n"
	for i := 0; i < (MaxBytes/len(line))+8; i++ {
		_, _ = sink.Write([]byte(line))
	}
	body := read(t, path)
	if len(body) > MaxBytes+len(cutNotice) {
		t.Fatalf("the file grew past the bound: %d bytes", len(body))
	}
	if !strings.HasSuffix(body, cutNotice) {
		t.Fatalf("the cut was not announced: %q", body[max(0, len(body)-80):])
	}
	before := len(body)
	_, _ = sink.Write([]byte(line))
	if read(t, path) != body || len(body) != before {
		t.Fatal("the file kept growing after the cut")
	}
}

func TestNoSinkWithoutAPath(t *testing.T) {
	t.Setenv(PathEnv, "")
	if Open() != nil {
		t.Fatal("a sink was opened with no path")
	}
	t.Setenv(PathEnv, "relative/live.log")
	if Open() != nil {
		t.Fatal("a relative path was accepted")
	}
	var absent *Sink
	if _, err := absent.Write([]byte("x")); err != nil {
		t.Fatalf("a nil sink must accept writes: %v", err)
	}
	absent.Close()
	var buffer strings.Builder
	if absent.Tee(&buffer) != &buffer {
		t.Fatal("a nil sink must not wrap the writer")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// A line that never ends is shown eventually, cut at a space: every secret
// shape is delimited by whitespace, so a cut there cannot split one. A cut
// at a fixed offset merely moved where the split happened (review of #187).
func TestAnEndlessLineIsCutAtASpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "stream.log")
	sink := openAt(t, path)
	secret := "TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	// A space before the token, and enough filler that the flush threshold
	// falls inside the token itself.
	filler := strings.Repeat("x", maxUnterminatedLine-20) + " "
	for i := 0; i < len(filler); i += 64 {
		end := i + 64
		if end > len(filler) {
			end = len(filler)
		}
		_, _ = sink.Write([]byte(filler[i:end]))
	}
	for i := 0; i < len(secret); i += 8 {
		end := i + 8
		if end > len(secret) {
			end = len(secret)
		}
		_, _ = sink.Write([]byte(secret[i:end]))
	}
	if body := read(t, path); strings.Contains(body, "ghp_") {
		t.Fatalf("part of the token was flushed: %q", body[max(0, len(body)-80):])
	}
	_, _ = sink.Write([]byte("\n"))
	body := read(t, path)
	if strings.Contains(body, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("the token reached the file whole")
	}
	if !strings.Contains(body, "[masked:") {
		t.Fatalf("the finished line was written without masking: %q", body[max(0, len(body)-80):])
	}
}

// The file holds whole lines only, whatever a step writes: a reader serves
// it a line at a time, so a region with no line in it could never be shown
// at all (review of #187).
func TestNoRegionWithoutALineSurvives(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "endless.log")
	sink := openAt(t, path)
	for i := 0; i < 6; i++ {
		_, _ = sink.Write([]byte(strings.Repeat("a", maxUnterminatedLine/2) + " "))
	}
	body := read(t, path)
	if body == "" {
		t.Fatal("nothing was flushed")
	}
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if len(line) > maxUnterminatedLine {
			t.Fatalf("a line of %d bytes cannot be served", len(line))
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Fatalf("the file does not end at a line: %q", body[max(0, len(body)-40):])
	}
}

// The bound belongs to the file: a step opens more than one sink on it (the
// agent's output and the model's answer), and a bound counted per sink let
// the file grow past it.
func TestTwoSinksShareOneBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live", "shared.log")
	t.Setenv(PathEnv, path)
	first, second := Open(), Open()
	line := strings.Repeat("a", 1024) + "\n"
	for i := 0; i < (MaxBytes/len(line))+8; i++ {
		_, _ = first.Write([]byte(line))
		_, _ = second.Write([]byte(line))
	}
	if size := len(read(t, path)); size > MaxBytes+2*len(cutNotice) {
		t.Fatalf("two sinks wrote %d bytes into a %d-byte bound", size, MaxBytes)
	}
}
