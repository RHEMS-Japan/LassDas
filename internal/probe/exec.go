package probe

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// cappedWriter keeps the first cap bytes and counts everything.
type cappedWriter struct {
	cap   int
	buf   bytes.Buffer
	total int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.total += n
	if remaining := w.cap - w.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.buf.Write(p)
	}
	// Always report the full length: a short write would stop the copy
	// from the child's pipe and kill the child with SIGPIPE, turning a
	// capped output into a failed measurement.
	return n, nil
}

func (w *cappedWriter) truncated() bool { return w.total > w.buf.Len() }

// execResult is what an executor hands back before recording.
type execResult struct {
	output    string
	total     int
	truncated bool
	exitCode  int
	timedOut  bool
	failure   string
}

// runExec runs the plan's argv directly — no shell, no inherited
// environment beyond what the session allows — and captures stdout and
// stderr together up to the probe's cap.
func runExec(ctx context.Context, plan Plan, env []string) execResult {
	timeout := time.Duration(plan.Spec.Timeout()) * time.Second
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	writer := &cappedWriter{cap: plan.Spec.MaxOutput()}
	command := exec.CommandContext(ctx, plan.Argv[0], plan.Argv[1:]...) // #nosec G204 -- argv is the catalogue's fixed shape with regex-checked slots
	command.Env = env
	command.Stdin = nil
	command.Stdout = writer
	command.Stderr = writer
	command.WaitDelay = 2 * time.Second
	err := command.Run()
	result := execResult{output: writer.buf.String(), total: writer.total, truncated: writer.truncated()}
	switch {
	case err == nil:
		result.exitCode = 0
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.exitCode = -1
		result.timedOut = true
		result.failure = "timed out"
	default:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.exitCode = exitErr.ExitCode()
		} else {
			result.exitCode = -1
			result.failure = err.Error()
		}
	}
	return result
}
