package probe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Limits bound one request's investigation.
type Limits struct {
	// MaxProbes counts every request the model makes, refusals included.
	MaxProbes int
	// MaxTotalBytes bounds the stored output across the request.
	MaxTotalBytes int
	// ExcerptBytes is how much of one output the model is shown.
	ExcerptBytes int
}

// DefaultLimits are the design's numbers (§3.1, §3.2).
var DefaultLimits = Limits{MaxProbes: 60, MaxTotalBytes: 16 * 1024 * 1024, ExcerptBytes: 32 * 1024}

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
	// Used counts requests so far, including those this session refused;
	// Bytes counts stored output. Both carry over from earlier rounds.
	Used  int
	Bytes int

	connector sqlConnector
	lookup    resolver
	now       func() time.Time
	httpHooks httpTestHooks
	// rotatedHosts are hosts that answered an http probe with a Set-Cookie
	// for a jar cookie; the session does not address them again (§3.2).
	rotatedHosts map[string]bool
}

// ErrBudgetExhausted ends the round honestly when the probe budget is spent.
var ErrBudgetExhausted = errors.New("probe budget exhausted")

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
		if s.rotatedHosts[plan.Spec.Hosts[0]] {
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
			s.rotatedHosts[plan.Spec.Hosts[0]] = true
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
		excerpt = excerpt[:limits.ExcerptBytes]
	}
	measurement.ExcerptBytes = len(excerpt)
	recorded, err := s.Recorder.Append(measurement)
	if err != nil {
		return Outcome{}, err
	}
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
