package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker"
)

// captureStderr collects what the process writes to stderr while during
// runs. The verb under test writes there directly, because the lines it
// writes are read back out of the step's captured stderr and out of nothing
// else.
func captureStderr(t *testing.T, during func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = write
	collected := make(chan string, 1)
	go func() {
		raw, _ := io.ReadAll(read)
		collected <- string(raw)
	}()
	defer func() {
		os.Stderr = original
		read.Close()
	}()
	during()
	write.Close()
	return <-collected
}

// Everything the round after a refused validation is told travels as a tail:
// the step keeps the last bytes of this verb's stderr, and the record keeps
// the last bytes of that. An output large enough to fill the window on its
// own pushes out whatever was written before it, so the line naming the
// command has to be written after the output and not before it.
//
// That line is the one that tells an install failure from a test failure.
// This verb runs the install command and the verify commands in one step, and
// a dependency that will not install and a test that will not pass print
// alike. One is the environment and one is the change, and the next round
// answers them differently.
func TestTheFailingCommandsNameSurvivesAnOversizedOutput(t *testing.T) {
	fixture := newTunedAgentFixture(t, editTheLabel, "true", func(binaries string, config *worker.Config) {
		writeStandInAgent(t, binaries, "stand-in-install", "exit 0")
		// More output than the window keeps, so the name is exactly what the
		// cut would have taken.
		writeStandInAgent(t, binaries, "stand-in-verify", `i=0
while [ $i -lt 400 ]; do
  echo "validation output line $i ......................................................"
  i=$((i+1))
done
exit 1`)
		config.Consumers[0].Mode.Toolchain = nil
		config.Consumers[0].Mode.InstallCommand = []string{"stand-in-install"}
		config.Consumers[0].Mode.VerifyCommands = [][]string{{"stand-in-verify"}}
	})
	if err := fixture.implement(t); err != nil {
		t.Fatalf("the fixture did not produce a candidate: %v", err)
	}

	var verbErr error
	stderr := captureStderr(t, func() {
		verbErr = run(context.Background(), []string{
			"run-validation", "--config", fixture.configPath, "--tool-sha", cliToolSHA,
			"--ticket", fixture.path("ticket.json"), "--source", fixture.path("source.json"),
			"--candidate", fixture.path("candidate.json"), "--repo-root", fixture.repoRoot,
			"--out", fixture.path("validation.json"),
		})
	})
	if verbErr == nil {
		t.Fatal("a failing verify command was accepted")
	}
	// The artifact is written on success alone, which is why the stderr above
	// is the only account of what went wrong.
	if _, err := os.Stat(fixture.path("validation.json")); err == nil {
		t.Fatal("a refused validation wrote its evidence artifact")
	}
	if !strings.Contains(stderr, "stand-in-verify") {
		t.Fatalf("the failing command was never named:\n%s", stderr)
	}
	// The fixture has to outgrow the window, or the cut this test is about
	// never happens and it would pass on any ordering.
	if len(stderr) <= worker.MaxValidationOutputBytes {
		t.Fatalf("the fixture printed %d bytes, which the cut never reaches", len(stderr))
	}
	kept := stderr[len(stderr)-worker.MaxValidationOutputBytes:]
	if !strings.Contains(kept, "validation command failed: stand-in-verify") {
		t.Fatalf("the name of the failing command falls outside the %d bytes that are kept:\n%s",
			worker.MaxValidationOutputBytes, kept[:200])
	}
}
