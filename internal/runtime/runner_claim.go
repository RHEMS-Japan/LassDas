package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/state"
)

// PullTask claims only the delivery assigned to the dispatched runner card.
// An unscoped Pull selects the project's current pending ticket, which may
// belong to a different card. That would also make claim recovery consult the
// wrong worker's liveness and let Prepare erase another delivery's records.
// Resolve the existing board/ledger binding; do not add another mapping store.
func (h *Hermes) PullTask(ctx context.Context, store *state.LocalStore, taskID, workspace string, request hook.PullClaimRequest) (hook.DispatchEnvelope, hook.PullDisposition, error) {
	refuse := func(err error) (hook.DispatchEnvelope, hook.PullDisposition, error) {
		return hook.DispatchEnvelope{}, "", fmt.Errorf("runner card binding: %w", err)
	}
	if taskID == "" {
		return refuse(errors.New("task id is required"))
	}
	tasks, err := h.ListTasks(ctx)
	if err != nil {
		return refuse(err)
	}
	var card *BoardTask
	for i := range tasks {
		if tasks[i].ID == taskID {
			if card != nil {
				return refuse(errors.New("task id is ambiguous"))
			}
			card = &tasks[i]
		}
	}
	if card == nil || card.Status != "running" || card.IdempotencyKey == "" {
		return refuse(errors.New("dispatched task is not a running card with a delivery id"))
	}
	if err := sameRunnerDirectory(workspace, card.WorkspacePath); err != nil {
		return refuse(fmt.Errorf("workspace differs from dispatched card: %w", err))
	}
	runs, err := store.ScanRuns(ctx)
	if err != nil {
		return refuse(err)
	}
	runID := ""
	for _, run := range runs {
		if run.DeliveryID != card.IdempotencyKey {
			continue
		}
		if runID != "" || !hook.ValidRunID(run.RunID) {
			return refuse(errors.New("card has no unique valid ledger run"))
		}
		var envelope hook.DispatchEnvelope
		if json.Unmarshal([]byte(run.EnvelopeJSON), &envelope) != nil || hook.ValidateEnvelope(envelope) != nil ||
			envelope.DeliveryID != card.IdempotencyKey || envelope.Snapshot.RunID != run.RunID ||
			envelope.Snapshot.SpaceKey != request.SpaceKey || envelope.Snapshot.ProjectID != request.ProjectID {
			return refuse(errors.New("card ledger envelope does not match this project's run"))
		}
		runID = run.RunID
	}
	if runID == "" || (request.RunID != "" && request.RunID != runID) {
		return refuse(errors.New("card delivery does not match a ledger run"))
	}
	// Check before claiming, clearing steps, reclaiming the tree, or reporting.
	// Old incorrectly bound workspaces need operator recovery, not erasure.
	if err := checkRunnerWorkspaceEnvelope(workspace, card.IdempotencyKey); err != nil {
		return refuse(err)
	}
	request.RunID = runID
	return store.Pull(ctx, request)
}

func sameRunnerDirectory(actual, expected string) error {
	for _, path := range []string{actual, expected} {
		if !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
			return errors.New("workspace must be an absolute non-root directory")
		}
	}
	actualInfo, err := os.Stat(actual)
	if err != nil {
		return err
	}
	expectedInfo, err := os.Stat(expected)
	if err != nil {
		return err
	}
	// SameFile accepts canonical aliases (e.g. /tmp and /private/tmp) without
	// accepting a different directory just because its final component matches.
	if !actualInfo.IsDir() || !expectedInfo.IsDir() || !os.SameFile(actualInfo, expectedInfo) {
		return errors.New("workspace directories do not identify the same directory")
	}
	return nil
}

func checkRunnerWorkspaceEnvelope(workspace, deliveryID string) error {
	path := filepath.Join(workspace, "ticket-envelope.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first dispatch, or a retry before Prepare wrote the envelope
	}
	if err != nil {
		return fmt.Errorf("read existing workspace envelope: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return errors.New("existing workspace envelope is not a bounded regular file; records preserved")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing workspace envelope: %w", err)
	}
	var envelope hook.DispatchEnvelope
	if json.Unmarshal(raw, &envelope) != nil || hook.ValidateEnvelope(envelope) != nil {
		return errors.New("existing workspace envelope is invalid; records preserved")
	}
	if envelope.DeliveryID != deliveryID {
		return errors.New("workspace contains another delivery's envelope; records preserved")
	}
	return nil
}
