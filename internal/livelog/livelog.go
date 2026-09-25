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

	"automation.internal/ticket-ingress/internal/cardsecret"
	"automation.internal/ticket-ingress/internal/probe"
)

// PathEnv names the file a step appends its live output to. The runner sets
// it per step; a process that does not have it writes nothing.
const PathEnv = "LASSDAS_LIVE_LOG"

// Dir is the run-directory subdirectory the files live in.
const Dir = "live"

// maxUnterminatedLine is how much of a line without an end is held before it
// is shown anyway.
const maxUnterminatedLine = 16 * 1024

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
	forced := false
	if end == 0 {
		// A line that never ends would never be shown; past a reasonable
		// length it is shown anyway, cut at the last space before the
		// bound. Every secret shape is delimited by whitespace, so a cut
		// there cannot split one - a cut at a fixed offset merely moved
		// where the split happened (review of #187).
		if len(s.pending) < maxUnterminatedLine {
			return len(p), nil
		}
		end = lastSpaceBefore(s.pending, maxUnterminatedLine)
		forced = true
	}
	chunk := string(s.pending[:end])
	s.pending = append([]byte(nil), s.pending[end:]...)
	if forced {
		// The file keeps whole lines only: a reader serves it a line at a
		// time, and a region with no line in it can never be served (review
		// of #187).
		chunk += "\n"
	}
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
	// The card's own credentials are named as forbidden literals, not only
	// the shapes a secret usually has: a connection string or a key file
	// this destination handed over has no shape the general masker knows,
	// and a step that prints one would publish it on the board.
	masked, _, refusal := probe.MaskSecrets(chunk, cardsecret.Literals())
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

// lastSpaceBefore is where a line may be cut without splitting a word: the
// last space at or before limit, or limit itself when the whole of it is one
// unbroken run (no secret shape this masks is that long).
func lastSpaceBefore(pending []byte, limit int) int {
	if limit > len(pending) {
		limit = len(pending)
	}
	for i := limit - 1; i > 0; i-- {
		switch pending[i] {
		case ' ', '\t', '\r':
			return i + 1
		}
	}
	return limit
}
