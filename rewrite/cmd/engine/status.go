package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	processingStatus = "processing"
	awaitingStatus   = "awaiting_requester"
	deliveredStatus  = "delivered"
	stoppedStatus    = "stopped"
)

func (s *statusConfig) id(kind string) int64 {
	if s == nil {
		return 0
	}
	switch kind {
	case processingStatus:
		return s.Processing
	case awaitingStatus:
		return s.AwaitingRequester
	case deliveredStatus:
		return s.Delivered
	case stoppedStatus:
		return s.Stopped
	}
	return 0
}

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
type statusRecord struct {
	Kind    string `json:"kind"`
	ID      int64  `json:"id"`
	Refused int    `json:"refused,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// applyStatus moves the issue to the status configured for this turn of the
// work, once per turn. A turn without a configured id changes nothing. A
// tracker that refuses is asked again on every tick for as long as the
// request lives; the same refusal is logged once.
func applyStatus(ctx context.Context, cfg config, issue sourceIssue, directory, kind string, observe func(string)) {
	id := cfg.Intake.Statuses.id(kind)
	if id == 0 {
		return
	}
	path := filepath.Join(directory, "status.json")
	var last statusRecord
	if raw, err := os.ReadFile(path); err == nil {
		if json.Unmarshal(raw, &last) == nil && last.Kind == kind && last.ID == id && last.Refused == 0 {
			return
		}
	}
	// Refusals are counted per turn: a new turn starts from zero.
	if last.Kind != kind || last.ID != id {
		last = statusRecord{Kind: kind, ID: id}
	}
	if err := cfg.Backlog.SetStatus(ctx, issue.Key, id); err != nil {
		if last.Reason != err.Error() {
			observe("status not set to " + kind + ", asked again each tick: " + err.Error())
		}
		last.Refused++
		last.Reason = err.Error()
		if data, err := json.Marshal(last); err == nil {
			writeRuntimeFile(path, data)
		}
		return
	}
	data, _ := json.Marshal(statusRecord{Kind: kind, ID: id})
	if err := writeRuntimeFile(path, data); err != nil {
		observe("status set to " + kind + " but not recorded: " + err.Error())
	}
}
