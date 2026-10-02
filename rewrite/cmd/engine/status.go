package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"ticket-runner/internal/tracker"
)

// statusConfig names, by tracker status id, where the runtime moves an
// issue at each turn of the work. Which status means what is the
// operator's; an id left at zero leaves that turn alone. The ids say whose
// move it is: the runtime's while it works, the requester's while a question
// waits, a person's once the work is delivered or stopped.
type statusConfig struct {
	Processing        int64 `json:"processing,omitempty"`
	AwaitingRequester int64 `json:"awaiting_requester,omitempty"`
	Delivered         int64 `json:"delivered,omitempty"`
	Stopped           int64 `json:"stopped,omitempty"`
}

const (
	processingStatus = tracker.Processing
	awaitingStatus   = tracker.AwaitingRequester
	deliveredStatus  = tracker.Delivered
	stoppedStatus    = tracker.Stopped
)

func (s *statusConfig) validate() error {
	if s == nil {
		return nil
	}
	for _, id := range []int64{s.Processing, s.AwaitingRequester, s.Delivered, s.Stopped} {
		if id < 0 {
			return errors.New("intake.statuses ids must not be negative")
		}
	}
	return nil
}

// statusRecord is what the runtime last set on the issue, kept beside the
// request so a restart does not set it again and a failed call is retried.
// The target is kept under "id", where the status id always was.
type statusRecord struct {
	Kind    string          `json:"kind"`
	Target  json.RawMessage `json:"id"`
	Refused int             `json:"refused,omitempty"`
	Reason  string          `json:"reason,omitempty"`
	// Next is when the refused move is asked again; see turnRecord.Next.
	Next time.Time `json:"next,omitempty"`
}

// applyStatus moves the issue to the status configured for this turn of the
// work, once per turn. A turn without a configured id changes nothing. A
// tracker that refuses is asked again on every tick for as long as the
// request lives; the same refusal is logged once.
func applyStatus(ctx context.Context, cfg config, issue sourceIssue, directory, kind string, observe func(string)) {
	source := cfg.source()
	target := source.Target(kind)
	if target == nil {
		return
	}
	path := filepath.Join(directory, "status.json")
	var last statusRecord
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &last) == nil && last.Kind == kind && bytes.Equal(last.Target, target) && last.Refused == 0 {
			return
		}
	}
	// Refusals are counted per turn: a new turn starts from zero.
	if last.Kind != kind || !bytes.Equal(last.Target, target) {
		last = statusRecord{Kind: kind, Target: target}
	}
	if last.Refused > 0 && turnClock().Before(last.Next) {
		return
	}
	if err := source.Move(ctx, issue, kind); err != nil {
		if last.Reason != err.Error() {
			observe("status not set to " + kind + ", asked again later: " + err.Error())
		}
		last.Refused++
		last.Reason = err.Error()
		last.Next = turnClock().Add(retryDelay(last.Refused))
		if data, err := json.Marshal(last); err == nil {
			writeRuntimeFile(path, data)
		}
		return
	}
	data, _ := json.Marshal(statusRecord{Kind: kind, Target: target})
	if err := writeRuntimeFile(path, data); err != nil {
		observe("status set to " + kind + " but not recorded: " + err.Error())
	}
}
