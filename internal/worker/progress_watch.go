package worker

import (
	"context"
	"strings"
	"sync"
	"time"
)

// A run that stopped making progress spends its whole budget before anything
// notices. Live, an implementer used 458 of its 500 iterations repeating the
// same search without changing a file, and a delivery ran 60 minutes into a
// failure. The only guard was the run's own time limit, which is the thing
// being spent.
//
// Nothing looked inside a running agent because looking cost a model call per
// look, and a watcher that costs more than the work is not a watcher. A model
// that answers a yes-or-no in under half a second, and charges nothing for
// its answer, makes the look affordable - so the look is what this adds.
//
// It ends a run; it never lets one continue past its own limits. A watcher
// that could extend a run would be a second budget nobody configured.

// ProgressJudge answers whether a run is still getting somewhere. It is the
// decision model in production and a script in tests; the watcher asks
// nothing else of it.
type ProgressJudge interface {
	// StillProgressing reports whether the run is advancing, and why the
	// answer is what it is. An error means no judgement was available: the
	// run continues, because a watcher that cannot see must not kill.
	StillProgressing(ctx context.Context, state string) (bool, string, error)
}

// ProgressWatch samples a run's output and ends the run when the judge says
// it has stopped getting anywhere.
type ProgressWatch struct {
	// Judge decides. Without one the watch does nothing at all.
	Judge ProgressJudge
	// Every is how often the run is looked at. A look costs one decision
	// call, so this is the only knob that costs money.
	Every time.Duration
	// Grace is how long a run is left alone before the first look. A run
	// that has not started producing output yet is not stalled.
	Grace time.Duration
	// Strikes is how many consecutive looks must say "not progressing"
	// before the run is ended. One look is a moment; three in a row, spread
	// over minutes, is a state.
	Strikes int
	// MaxStateBytes bounds what one look reads of the output. The tail is
	// what matters: a loop repeats at the end, not at the beginning.
	MaxStateBytes int
}

// StalledError is why a run was ended. It carries the judge's own words so
// the report says what stopped, not merely that something did.
type StalledError struct {
	Reason string
	Looks  int
}

func (e *StalledError) Error() string {
	return "the run stopped making progress and was ended: " + e.Reason
}

// progressDefaults fill in a watch that named only a judge. They are
// deliberately slow: the cost of ending a live run early is higher than the
// cost of one more look.
const (
	defaultProgressEvery   = 2 * time.Minute
	defaultProgressGrace   = 5 * time.Minute
	defaultProgressStrikes = 3
	defaultProgressState   = 8 * 1024
)

func (w ProgressWatch) withDefaults() ProgressWatch {
	if w.Every <= 0 {
		w.Every = defaultProgressEvery
	}
	if w.Grace <= 0 {
		w.Grace = defaultProgressGrace
	}
	if w.Strikes <= 0 {
		w.Strikes = defaultProgressStrikes
	}
	if w.MaxStateBytes <= 0 {
		w.MaxStateBytes = defaultProgressState
	}
	return w
}

// transcriptReader hands the watcher the output so far. The buffer the run
// writes into is read from another goroutine, so the reader carries its own
// lock.
type transcriptReader interface{ Tail(limit int) string }

// Watch looks at the run until ctx ends. It returns the reason the run was
// ended, or nil when the run finished on its own or no judgement was ever
// available. stop is called once, with the reason, when the run is ended.
func (w ProgressWatch) Watch(ctx context.Context, output transcriptReader, stop func(*StalledError)) {
	w = w.withDefaults()
	if w.Judge == nil || output == nil || stop == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(w.Grace):
	}
	ticker := time.NewTicker(w.Every)
	defer ticker.Stop()
	strikes, looks := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		tail := output.Tail(w.MaxStateBytes)
		if strings.TrimSpace(tail) == "" {
			// Nothing has been written at all. That is not a judgement the
			// model can make, and killing on silence alone would end a run
			// whose tool is simply slow.
			continue
		}
		looks++
		progressing, reason, err := w.Judge.StillProgressing(ctx, tail)
		if err != nil {
			// No judgement was available. A watcher that cannot see must
			// not kill, and the strikes it already counted are stale.
			strikes = 0
			continue
		}
		if progressing {
			strikes = 0
			continue
		}
		strikes++
		if strikes >= w.Strikes {
			stop(&StalledError{Reason: reason, Looks: looks})
			return
		}
	}
}

// lockedTranscript is a bytes buffer the run appends to and the watcher
// reads from. The run's writer and the watcher's reader are different
// goroutines, so every access takes the lock.
type lockedTranscript struct {
	mu   sync.Mutex
	text strings.Builder
}

func (t *lockedTranscript) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.Write(p)
}

func (t *lockedTranscript) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.text.String()
}

// Tail is the end of the output, which is where a loop repeats.
func (t *lockedTranscript) Tail(limit int) string {
	text := t.String()
	if limit > 0 && len(text) > limit {
		return text[len(text)-limit:]
	}
	return text
}

// DecisionProgressJudge asks the decision model whether a run is advancing.
// Two questions travel in one call, because they fail differently: a run can
// be busy and repeating itself, and it can be quiet and still thinking.
type DecisionProgressJudge struct {
	Client   *SystemOneClient
	Endpoint ModelEndpoint
	// Threshold is how sure the model must be before a look counts against
	// the run. The watch needs several such looks in a row, so this is the
	// inner of two guards, not the only one.
	Threshold float64
}

// defaultStalledThreshold is deliberately high: ending a live delivery early
// costs more than one more look.
const defaultStalledThreshold = 0.8

const (
	stalledRepeatQuestion = "repeating"
	stalledChangeQuestion = "changing_nothing"
)

func (j DecisionProgressJudge) StillProgressing(ctx context.Context, state string) (bool, string, error) {
	if j.Client == nil {
		return true, "", safeModelLiteral("no decision model is configured to watch this run")
	}
	threshold := j.Threshold
	if threshold <= 0 {
		threshold = defaultStalledThreshold
	}
	answers, err := j.Client.Decide(ctx, j.Endpoint, SystemOneRequest{
		Model: j.Endpoint.Model,
		State: state,
		Questions: map[string]SystemOneQuestion{
			stalledRepeatQuestion: {
				Kind: SystemOneNoul,
				Instructions: "This is the tail of a coding agent's output. " +
					"It is repeating itself: running the same searches or reading the same files over and over, " +
					"without the repetitions telling it anything new.",
			},
			stalledChangeQuestion: {
				Kind: SystemOneNoul,
				Instructions: "This is the tail of a coding agent's output. " +
					"It has stopped changing the repository: it is only looking, not editing, " +
					"and the looking is no longer narrowing anything down.",
			},
		},
	})
	if err != nil {
		return true, "", err
	}
	repeating, changing := answers[stalledRepeatQuestion], answers[stalledChangeQuestion]
	if repeating.Noul == nil || changing.Noul == nil {
		return true, "", safeModelLiteral("the watch got no probability back")
	}
	// Both must hold. A run that repeats its searches while still editing is
	// working; a run that only reads while narrowing something down is
	// working too. Stalled is the pair.
	if *repeating.Noul >= threshold && *changing.Noul >= threshold {
		return false, "same searches repeated without changing anything", nil
	}
	return true, "", nil
}
