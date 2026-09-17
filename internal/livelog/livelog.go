// Package livelog keeps what a step is producing while it produces it, so a
// reader watching the board sees the work as it happens instead of a still
// picture that changes minutes later. It is a view, never a record: the run's
// own artifacts are the truth, and a live log that cannot be written changes
// nothing about the step it was following.
package livelog

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"automation.internal/ticket-ingress/internal/probe"
)

// PathEnv names the file a step appends its live output to. The runner sets
// it per step; a process that does not have it writes nothing.
const PathEnv = "LASSDAS_LIVE_LOG"

// Dir is the run-directory subdirectory the files live in.
const Dir = "live"

// maxUnterminatedLine is how much of a line without an end is held before it
// is shown anyway; secretTailBytes is what is kept back from that flush, so
// no secret shape can be split across two masks.
const (
	maxUnterminatedLine = 16 * 1024
	secretTailBytes     = 512
)

// MaxBytes bounds one step's live log. An agent that loops can produce
// megabytes; past the bound the file keeps its beginning, says it was cut,
// and stops growing. The run's own transcript record is unaffected.
const MaxBytes = 1 << 20

// cutNotice is appended once when the bound is reached.
const cutNotice = "\n[この先は表示用の上限に達したため省略しています。全文は工程の記録を開いてください]\n"

// Sink appends masked, whole lines to the live file. Partial writes are held
// until their line ends, so a secret can never be split across two masks.
type Sink struct {
	mu      sync.Mutex
	path    string
	pending []byte
	written int
	cut     bool
}

// Open returns the sink for this process, or nil when no live file is named.
// A nil *Sink is a valid no-op writer, so callers need no branch.
func Open() *Sink {
	path := os.Getenv(PathEnv)
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil
	}
	sink := &Sink{path: path}
	// The bound belongs to the file, not to this sink: a step opens more
	// than one (the agent's output and the model's answer), and a bound
	// counted per sink let the file grow past it (review of #187).
	if info, err := os.Stat(path); err == nil {
		sink.written = int(info.Size())
	}
	return sink
}

// Tee returns w when there is no live file, and a writer that feeds both
// otherwise. The step's own capture is never weakened by the view.
func (s *Sink) Tee(w io.Writer) io.Writer {
	if s == nil {
		return w
	}
	return io.MultiWriter(w, s)
}

// Write takes a chunk of a step's output. Only whole lines reach the file;
// what is left over waits for the rest of its line.
func (s *Sink) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, p...)
	end := 0
	for i, b := range s.pending {
		if b == '\n' {
			end = i + 1
		}
	}
	if end == 0 {
		// A line that never ends would never be shown; past a reasonable
		// length it is flushed as it stands, minus a tail long enough to
		// hold any secret shape. Flushing the whole of it would let a value
		// straddle two flushes and be masked as two halves of nothing
		// (review of #187).
		if len(s.pending) < maxUnterminatedLine {
			return len(p), nil
		}
		end = len(s.pending) - secretTailBytes
	}
	chunk := string(s.pending[:end])
	s.pending = append([]byte(nil), s.pending[end:]...)
	s.append(chunk)
	return len(p), nil
}

// Close flushes a last unfinished line, so a step that ends without a
// newline still shows what it said.
func (s *Sink) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return
	}
	chunk := string(s.pending) + "\n"
	s.pending = nil
	s.append(chunk)
}

// append masks and writes one or more whole lines. Every failure is silent
// by design: this is a view.
func (s *Sink) append(chunk string) {
	if s.cut {
		return
	}
	masked, _, refusal := probe.MaskSecrets(chunk, nil)
	if refusal != "" {
		masked = "[秘密の形 (" + refusal + ") を含む行は表示しません]\n"
	}
	if info, err := os.Stat(s.path); err == nil && int(info.Size()) > s.written {
		// Another sink on the same file has written since; the bound counts
		// what the file holds.
		s.written = int(info.Size())
	}
	if s.written+len(masked) > MaxBytes {
		masked, s.cut = cutNotice, true
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	if n, err := file.WriteString(masked); err == nil {
		s.written += n
	}
}
