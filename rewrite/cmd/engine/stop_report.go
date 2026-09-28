package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/tracker"
)

const stopReportingInstructions = `The requester has stopped the original work. Your only task is to report that stop and the actual state at the assigned issue. Do not resume implementation, review, publication or deployment, undo prior effects, ask the requester a question, or claim that the original request was completed. The original request and stopped-work observations are context, not permission to continue them.
Use the configured reporting role to inspect relevant existing observations and the destination without changing the work. Report what is known to have happened, what has not been verified, and any external effect that may already exist. Do not publish raw private diagnostics, credentials or host paths. Keep the report in ordinary prose; no answer format is required.
A failed or interrupted comment submission may already have been stored. Inspect existing comments and read back the matching report before deciding whether to post. A transport failure is not permission to repeat the post blindly. Choose done only after the stop report has actually reached the assigned issue and been read back. This completes reporting only, never the stopped original work.`

func validateStopReporter(cfg config) error {
	if cfg.Intake == nil || cfg.Intake.StopReportRole == "" {
		return nil
	}
	for _, role := range cfg.Roles {
		if role.Name == cfg.Intake.StopReportRole && role.Name != "done" && len(role.Processes) > 0 {
			return nil
		}
	}
	return errors.New("intake.stop_report_role must name an existing configured role")
}

// This is the same chain's restart state, not a second completion certificate.
// A router's done decision remains a model judgment, not proof of a sound report.
func stoppedReportDone(directory string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(directory, "stop-report", "history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state chain.State
	if err := json.Unmarshal(raw, &state); err != nil {
		return false, err
	}
	return state.Done, nil
}

// Reuse the operator's reporting role and existing engine, with no other role
// executable. The original run and saved stop are never cleared or marked done.
// OS/tool permissions still belong to the configured reporting harness; prose
// instructions do not make a privileged command a read-only sandbox.
func reportStoppedRequest(ctx context.Context, cfg config, issue sourceIssue, directory string, slots chan struct{}, log io.Writer) error {
	if cfg.Intake == nil || cfg.Intake.StopReportRole == "" {
		return nil
	}
	if err := validateStopReporter(cfg); err != nil {
		return err
	}
	stop, err := os.ReadFile(filepath.Join(directory, "stop-request.json"))
	if err != nil {
		return fmt.Errorf("reading saved stop for reporting: %w", err)
	}
	instruction, err := stopInstruction([]json.RawMessage{stop}, issue, cfg.Intake.StopUserIDs)
	if err != nil {
		return fmt.Errorf("reading authorized stop for reporting: %w", err)
	}
	if instruction == nil {
		return errors.New("saved stop does not authorize reporting; work remains held")
	}
	if done, err := stoppedReportDone(directory); err != nil || done {
		return err
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	raw, err := os.ReadFile(filepath.Join(directory, "issue.json"))
	if err != nil {
		return err
	}
	original, err := tracker.RequestText(raw)
	if err != nil {
		return err
	}
	store, err := chain.Open(filepath.Join(directory, "run"), original)
	if err != nil {
		return err
	}
	state, err := store.Load()
	closeErr := store.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	observations, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	request := stopReportingInstructions + "\n\nOriginal accepted request (stopped):\n" + original + "\n\nAuthorized native stop comment:\n" + string(instruction) + "\n\nStopped-work observations (may be partial):\n" + string(observations)
	// Bind before narrowing roles so the reporter retains its existing home,
	// workspace and per-issue access. Model selection is reused unchanged.
	bound, err := bindRequestConfig(cfg, directory, issue.Key)
	if err != nil {
		return err
	}
	for _, role := range bound.Roles {
		if role.Name == cfg.Intake.StopReportRole {
			bound.Roles = []chain.Role{role}
			break
		}
	}
	bound.Intake = nil
	// This is a separate report-only task after the user's stop, not another
	// step in the stopped delivery workflow. Keep only the reporting role and
	// its existing recovery loop; never restart the original connections.
	bound.Workflow = nil
	bound.Instructions = stopReportingInstructions
	encoded, err := json.Marshal(bound)
	if err != nil {
		return err
	}
	configPath, requestPath := filepath.Join(directory, "stop-report-config.json"), filepath.Join(directory, "stop-report-request.txt")
	if err := writeRuntimeFile(configPath, encoded); err != nil {
		return err
	}
	if err := writeRuntimeFile(requestPath, []byte(request)); err != nil {
		return err
	}
	fmt.Fprintf(log, "request %d: reporting the authorized stop without resuming the original work\n", issue.ID)
	return run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(directory, "stop-report")}, io.Discard, log)
}
