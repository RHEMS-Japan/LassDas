package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits bound one request's investigation.
type Limits struct {
	// MaxProbes counts every request the model makes, refusals included.
	MaxProbes int
	// MaxTotalBytes bounds the stored output across the request.
	MaxTotalBytes int
	// ExcerptBytes is how much of one output the model is shown at a
	// time: the excerpt a measurement returns, and each window Read shows.
	ExcerptBytes int
	// MaxReads counts the windows the model asks for beyond the excerpts
	// (Read). Reads run no probe and store nothing; the cap only keeps a
	// round from paging for ever.
	MaxReads int
}

// DefaultLimits are the design's numbers (§3.1, §3.2).
var DefaultLimits = Limits{MaxProbes: 60, MaxTotalBytes: 16 * 1024 * 1024, ExcerptBytes: 32 * 1024, MaxReads: 120}

// Session executes requests for one investigation round and records every
// outcome. It holds what the model must never hold: the environment for
// exec probes, the DSNs, the jar.
type Session struct {
	Catalog  Catalog
	Recorder *Recorder
	Limits   Limits
	// Env is the environment exec probes run with (the kernel's choice, not
	// the process's whole environment).
	Env []string
	// RepoRoot is the baseline working copy for the built-in repo probes.
	RepoRoot string
	// DSN looks a sql probe's connection string up by its dsn_env name.
	DSN func(name string) string
	// Jar is the observation session, attached read-only to http probes
	// that declare it. Its values are also refused in any output.
	Jar []Cookie
	// Used counts requests so far, including those this session refused,
	// and carries over from earlier rounds; Bytes counts the output this
	// round stored; Reads counts the read requests this round made (Read),
	// refusals included.
	Used  int
	Bytes int
	Reads int

	connector sqlConnector
	lookup    resolver
	now       func() time.Time
	httpHooks httpTestHooks
	// rotatedHosts are hosts that answered an http probe with a Set-Cookie
	// for a jar cookie; the session does not address them again (§3.2).
	rotatedHosts map[string]bool
	// records keeps the measurements this session recorded or read, so a
	// window is served without re-verifying the whole record each time.
	records map[string]Measurement
}

// ErrBudgetExhausted ends the round honestly when the probe budget is spent.
var ErrBudgetExhausted = errors.New("probe budget exhausted")

// ErrReadBudgetExhausted tells the model no more windows will be shown.
var ErrReadBudgetExhausted = errors.New("read budget exhausted")

// ErrReadRefused marks a read the kernel would not serve — an id that is
// not recorded, an offset outside the record or inside a character, a
// refused measurement — as the model's mistake to correct, apart from the
// kernel's own failures.
var ErrReadRefused = errors.New("refused")

// Window is one stretch of a recorded output beyond the excerpt the model
// was shown: what the model asked to read, cut on a character boundary, and
// where the record continues.
type Window struct {
	ID         string `json:"id"`
	Offset     int    `json:"offset"`
	Bytes      int    `json:"bytes"`
	NextOffset int    `json:"next_offset"`
	// StoredBytes is the end of what can be read. Truncated says the
	// probe's own cap cut the output before it was stored, so OutputBytes
	// on the measurement is larger and the tail exists nowhere.
	StoredBytes int    `json:"stored_bytes"`
	Remaining   int    `json:"remaining"`
	Truncated   bool   `json:"truncated,omitempty"`
	Text        string `json:"text"`
}

// Read shows the model the window of a recorded output that starts at
// offset, ExcerptBytes long at most. A measurement's excerpt is the prefix
// of exactly what is stored, so a role that needs the rest — an event list
// longer than the excerpt, live 2026-09-07: three rounds could not converge
// on a count the reviewer read from the full record — pages through the
// same stored bytes instead of re-running the probe. Nothing is executed
// or recorded; the budget only bounds the paging.
func (s *Session) Read(id string, offset int) (Window, error) {
	if s.Recorder == nil {
		return Window{}, errors.New("session has no recorder")
	}
	limits := s.Limits
	if limits.ExcerptBytes <= 0 {
		limits.ExcerptBytes = DefaultLimits.ExcerptBytes
	}
	if limits.MaxReads <= 0 {
		limits.MaxReads = DefaultLimits.MaxReads
	}
	if s.Reads >= limits.MaxReads {
		return Window{}, ErrReadBudgetExhausted
	}
	s.Reads++
	measurement, err := s.measurementByID(id)
	if err != nil {
		return Window{}, err
	}
	if measurement.Refused {
		return Window{}, fmt.Errorf("%w: measurement %s was refused; nothing is stored", ErrReadRefused, id)
	}
	if offset < 0 || offset >= len(measurement.Output) {
		return Window{}, fmt.Errorf("%w: offset %d is outside the stored output of %s (%d bytes)", ErrReadRefused, offset, id, len(measurement.Output))
	}
	if !utf8.RuneStart(measurement.Output[offset]) {
		return Window{}, fmt.Errorf("%w: offset %d of %s is inside a character; start at this record's excerpt_bytes (%d) or a next_offset", ErrReadRefused, offset, id, measurement.ExcerptBytes)
	}
	end := characterBoundary(measurement.Output, offset+limits.ExcerptBytes)
	if end <= offset {
		return Window{}, fmt.Errorf("%w: the window at %d of %s would be empty", ErrReadRefused, offset, id)
	}
	text := measurement.Output[offset:end]
	return Window{
		ID: id, Offset: offset, Bytes: len(text), NextOffset: end,
		StoredBytes: len(measurement.Output), Remaining: len(measurement.Output) - end,
		Truncated: measurement.Truncated, Text: text,
	}, nil
}

// characterBoundary moves a cut at end back to the start of the character
// it would split, so an excerpt and the windows after it are the stored
// output byte for byte and no character is lost between two of them.
func characterBoundary(output string, end int) int {
	if end >= len(output) {
		return len(output)
	}
	for end > 0 && !utf8.RuneStart(output[end]) {
		end--
	}
	return end
}

// measurementByID serves a read from this session's copy of the measurement
// when it recorded (or already read) it, and from the verified record
// otherwise.
func (s *Session) measurementByID(id string) (Measurement, error) {
	if measurement, ok := s.records[id]; ok {
		return measurement, nil
	}
	measurement, err := s.Recorder.Lookup(id)
	if err != nil {
		return Measurement{}, err
	}
	s.remember(measurement)
	return measurement, nil
}

func (s *Session) remember(measurement Measurement) {
	if s.records == nil {
		s.records = map[string]Measurement{}
	}
	s.records[measurement.ID] = measurement
}

// Outcome is what the model is told: the recorded measurement (without its
// full output) and the excerpt it may read.
type Outcome struct {
	Measurement Measurement
	Excerpt     string
}

// Run resolves, executes and records one request. Refusals are recorded
// like measurements so the attempt stays visible. The returned error is
// only for the kernel's own failures (the record could not be written) or
// for an exhausted budget; a refused request is not an error.
func (s *Session) Run(ctx context.Context, request Request) (Outcome, error) {
	if s.Recorder == nil {
		return Outcome{}, errors.New("session has no recorder")
	}
	limits := s.Limits
	if limits.MaxProbes <= 0 {
		limits.MaxProbes = DefaultLimits.MaxProbes
	}
	if limits.MaxTotalBytes <= 0 {
		limits.MaxTotalBytes = DefaultLimits.MaxTotalBytes
	}
	if limits.ExcerptBytes <= 0 {
		limits.ExcerptBytes = DefaultLimits.ExcerptBytes
	}
	if s.Used >= limits.MaxProbes {
		return Outcome{}, ErrBudgetExhausted
	}
	s.Used++
	started := s.clock()
	measurement := Measurement{Probe: request.Probe, Args: request.Args, StartedAt: started}

	plan, refusal := s.Catalog.Resolve(request)
	if refusal != nil {
		return s.record(measurement, execResult{exitCode: -1, failure: refusal.Reason}, true, limits)
	}
	measurement.Args = plan.Args
	measurement.Description = plan.Spec.Description

	var result execResult
	rotated := false
	switch plan.Spec.Kind {
	case KindExec:
		result = runExec(ctx, plan, s.Env)
	case KindRepo:
		result = runRepo(plan, s.RepoRoot)
	case KindSQL:
		dsn := ""
		if s.DSN != nil {
			dsn = s.DSN(plan.Spec.DSNEnv)
		}
		connector := s.connector
		if connector == nil {
			connector = pgxConnector{}
		}
		result = runSQL(ctx, plan, dsn, connector)
	case KindHTTP:
		if s.rotatedHosts[plan.Args["host"]] {
			result = execResult{exitCode: -1, failure: "refused: the host rotated the session cookie earlier in this request; it is not addressed again"}
			break
		}
		lookup := s.lookup
		if lookup == nil {
			lookup = systemResolver
		}
		var http httpProbeResult
		http, result = runHTTPWith(ctx, plan, s.Jar, lookup, s.httpHooks)
		rotated = http.rotated
		if rotated {
			if s.rotatedHosts == nil {
				s.rotatedHosts = map[string]bool{}
			}
			s.rotatedHosts[plan.Args["host"]] = true
		}
	default:
		result = execResult{exitCode: -1, failure: "kind has no executor"}
	}
	refused := result.failure != "" && len(result.failure) > 8 && result.failure[:8] == "refused:"
	measurement.Rotated = rotated
	return s.record(measurement, result, refused, limits)
}

func (s *Session) record(measurement Measurement, result execResult, refused bool, limits Limits) (Outcome, error) {
	measurement.EndedAt = s.clock()
	measurement.ExitCode = result.exitCode
	measurement.Refused = refused
	measurement.Reason = result.failure
	if result.timedOut && measurement.Reason == "" {
		measurement.Reason = "timed out"
	}
	// Normalise before the excerpt is cut so the model sees a prefix of
	// exactly what is stored.
	result.output = strings.ToValidUTF8(result.output, "\uFFFD")
	if !refused {
		if kind, found := SecretShaped(result.output, s.forbiddenLiterals()); found {
			// The output is not kept; the attempt is.
			measurement.Refused = true
			measurement.Reason = fmt.Sprintf("refused: output carried a %s and was not stored", kind)
			result.output = ""
			result.total = 0
			result.truncated = false
		}
	}
	if s.Bytes+len(result.output) > limits.MaxTotalBytes {
		measurement.Refused = true
		measurement.Reason = "refused: the request's output budget is spent; output not stored"
		result.output = ""
		result.truncated = false
	}
	measurement.Output = result.output
	measurement.OutputBytes = result.total
	measurement.Truncated = result.truncated
	excerpt := result.output
	if limits.ExcerptBytes > 0 && len(excerpt) > limits.ExcerptBytes {
		// Cut on a character boundary: excerpt_bytes is where a read of
		// the rest starts, and a character split between the two would be
		// lost from both.
		excerpt = excerpt[:characterBoundary(result.output, limits.ExcerptBytes)]
	}
	measurement.ExcerptBytes = len(excerpt)
	recorded, err := s.Recorder.Append(measurement)
	if err != nil {
		return Outcome{}, err
	}
	s.remember(recorded)
	s.Bytes += len(recorded.Output)
	told := recorded
	told.Output = ""
	return Outcome{Measurement: told, Excerpt: excerpt}, nil
}

func (s *Session) forbiddenLiterals() []string {
	out := make([]string, 0, len(s.Jar))
	for _, cookie := range s.Jar {
		out = append(out, cookie.Value)
	}
	return out
}

func (s *Session) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now().UTC()
}

// ExecEnvironmentNames are the only variables exec probes inherit from the
// kernel: the path, the home, and the read-only cloud identities' pointers.
var ExecEnvironmentNames = []string{"PATH", "HOME", "KUBECONFIG", "AWS_PROFILE", "AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "AWS_REGION", "AWS_DEFAULT_REGION", "AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"}

// EnvFromProcess picks the named variables out of the process environment
// for exec probes; nothing else is inherited.
func EnvFromProcess(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			out = append(out, name+"="+value)
		}
	}
	return out
}
