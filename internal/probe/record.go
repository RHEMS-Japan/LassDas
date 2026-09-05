package probe

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Measurement is one line of measurements.jsonl: what was asked, what the
// kernel ran, what came back, and the chain value that lets a prefix of the
// file be checked as one unit. The role never writes these; the kernel does.
type Measurement struct {
	ID        string            `json:"id"`
	Probe     string            `json:"probe"`
	Args      map[string]string `json:"args,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	ExitCode  int               `json:"exit_code"`
	// Output is the captured text, cut at the probe's cap. OutputBytes is
	// the full length before the cut; Truncated says a cut happened.
	Output       string `json:"output,omitempty"`
	OutputBytes  int    `json:"output_bytes"`
	Truncated    bool   `json:"truncated,omitempty"`
	OutputSHA256 string `json:"output_sha256"`
	// ExcerptBytes is how much of Output the model was shown.
	ExcerptBytes int `json:"excerpt_bytes"`
	// Refused marks a request the catalogue or the guard did not execute;
	// Reason says why. Rotated marks an http response that tried to change
	// the shared jar.
	Refused bool   `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Rotated bool   `json:"rotated,omitempty"`

	LineSHA256  string `json:"line_sha256"`
	ChainSHA256 string `json:"chain_sha256"`
}

// Recorder appends measurements to measurements.jsonl. Reopening an
// existing file verifies its chain and continues it, so a later round of
// the same delivery extends the record without touching what a sealed
// report already fingerprinted.
type Recorder struct {
	path  string
	count int
	chain string
}

// OpenRecorder opens or creates the file and verifies the existing chain.
func OpenRecorder(path string) (*Recorder, error) {
	recorder := &Recorder{path: path}
	file, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("measurements: %w", err)
	}
	defer file.Close()
	count, chain, err := verifyChain(file, -1)
	if err != nil {
		return nil, err
	}
	recorder.count, recorder.chain = count, chain
	return recorder, nil
}

// Count is the number of lines recorded so far.
func (r *Recorder) Count() int { return r.count }

// Chain is the chain value after the last line ("" for an empty file).
func (r *Recorder) Chain() string { return r.chain }

// NextID is the id the next appended measurement will carry.
func (r *Recorder) NextID() string { return measurementID(r.count + 1) }

func measurementID(n int) string { return fmt.Sprintf("m-%04d", n) }

// Append assigns the id, computes the fingerprints and writes one line.
func (r *Recorder) Append(measurement Measurement) (Measurement, error) {
	measurement.ID = measurementID(r.count + 1)
	// JSON can carry only valid UTF-8: an output cut inside a multibyte
	// character would be escaped on the way out and come back different,
	// and the line's fingerprint would never re-derive. Normalise first,
	// hash what is stored.
	measurement.Output = strings.ToValidUTF8(measurement.Output, "\uFFFD")
	measurement.Reason = strings.ToValidUTF8(measurement.Reason, "\uFFFD")
	measurement.OutputSHA256 = digestHex([]byte(measurement.Output))
	measurement.LineSHA256, measurement.ChainSHA256 = "", ""
	encoded, err := json.Marshal(measurement)
	if err != nil {
		return Measurement{}, err
	}
	measurement.LineSHA256 = digestHex(encoded)
	measurement.ChainSHA256 = chainValue(r.chain, measurement.LineSHA256)
	line, err := json.Marshal(measurement)
	if err != nil {
		return Measurement{}, err
	}
	file, err := os.OpenFile(r.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return Measurement{}, fmt.Errorf("measurements: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return Measurement{}, fmt.Errorf("measurements: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return Measurement{}, fmt.Errorf("measurements: %w", err)
	}
	if err := file.Close(); err != nil {
		return Measurement{}, fmt.Errorf("measurements: %w", err)
	}
	r.count++
	r.chain = measurement.ChainSHA256
	return measurement, nil
}

// chainValue links one line to everything before it.
func chainValue(previous, line string) string {
	return digestHex([]byte(previous + line))
}

func digestHex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// maxLineBytes bounds one stored line: the largest permitted output
// (4 × DefaultMaxOutputBytes) escaped at the worst JSON ratio (6 ×) plus
// the record's own fields. A writer can never produce a line the reader
// refuses.
const maxLineBytes = 4*DefaultMaxOutputBytes*6 + 64*1024

// ErrChainBroken reports a line whose fingerprint or chain value does not
// re-derive: the file was edited, reordered or truncated in the middle.
var ErrChainBroken = errors.New("measurements chain is broken")

// VerifyPrefix re-derives the chain over the first n lines of the file and
// returns the chain value after line n. A sealed report that recorded
// (count, chain) verifies against a file that later rounds appended to.
func VerifyPrefix(path string, n int) (string, error) {
	if n < 0 {
		return "", errors.New("measurements: prefix length is negative")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("measurements: %w", err)
	}
	defer file.Close()
	count, chain, err := verifyChain(file, n)
	if err != nil {
		return "", err
	}
	if count < n {
		return "", fmt.Errorf("%w: only %d of %d lines present", ErrChainBroken, count, n)
	}
	return chain, nil
}

// ReadPrefix returns the first n measurements after verifying their chain.
func ReadPrefix(path string, n int) ([]Measurement, error) {
	if _, err := VerifyPrefix(path, n); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("measurements: %w", err)
	}
	defer file.Close()
	var out []Measurement
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	for scanner.Scan() && len(out) < n {
		var measurement Measurement
		if err := json.Unmarshal(scanner.Bytes(), &measurement); err != nil {
			return nil, fmt.Errorf("measurements: %w", err)
		}
		out = append(out, measurement)
	}
	return out, scanner.Err()
}

// verifyChain walks the file, re-deriving every line's fingerprint and the
// chain, and stops after limit lines (limit < 0 walks everything).
func verifyChain(reader io.Reader, limit int) (int, string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	count, chain := 0, ""
	for scanner.Scan() {
		if limit >= 0 && count >= limit {
			break
		}
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var measurement Measurement
		if err := json.Unmarshal(raw, &measurement); err != nil {
			return count, chain, fmt.Errorf("%w: line %d is not a measurement", ErrChainBroken, count+1)
		}
		if measurement.ID != measurementID(count+1) {
			return count, chain, fmt.Errorf("%w: line %d carries id %q", ErrChainBroken, count+1, measurement.ID)
		}
		recordedLine, recordedChain := measurement.LineSHA256, measurement.ChainSHA256
		measurement.LineSHA256, measurement.ChainSHA256 = "", ""
		encoded, err := json.Marshal(measurement)
		if err != nil {
			return count, chain, err
		}
		if digestHex(encoded) != recordedLine {
			return count, chain, fmt.Errorf("%w: line %d fingerprint does not re-derive", ErrChainBroken, count+1)
		}
		if chainValue(chain, recordedLine) != recordedChain {
			return count, chain, fmt.Errorf("%w: line %d chain value does not re-derive", ErrChainBroken, count+1)
		}
		count++
		chain = recordedChain
	}
	if err := scanner.Err(); err != nil {
		return count, chain, fmt.Errorf("measurements: %w", err)
	}
	return count, chain, nil
}
