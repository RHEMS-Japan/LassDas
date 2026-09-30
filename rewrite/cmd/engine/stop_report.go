package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

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
// reportRecords bounds how many records the stopped request's reporter reads,
// and reportFieldRunes how much of each record's text: a report reads the
// opening of what a role wrote and the end of what failed, as a role's own
// prompt does, and the whole record stays on disk for anyone who needs it.
const reportRecords, reportFieldRunes = 60, 4000

// stopObservations renders the stopped request's record for its reporter. A
// launch that failed the same way over and over becomes one entry that says
// how often it repeated, but every runtime note that says something new, such
// as a receipt read back from a process that still had an effect, is kept:
// the report exists to name effects that may already exist.
func stopObservations(state chain.State) ([]byte, error) {
	history, counts := reportHistory(state.History)
	for i := range history {
		record := &history[i]
		record.Output = headRunes(record.Output, reportFieldRunes)
		record.Instruction = headRunes(record.Instruction, reportFieldRunes/4)
		record.Diagnostics = tailRunes(record.Diagnostics, reportFieldRunes)
		record.Error = tailRunes(record.Error, reportFieldRunes)
		if count := counts[i]; count > 0 {
			record.Error = fmt.Sprintf("(this failure repeated %d times in a row; this is the latest)\n%s", count+1, record.Error)
		}
	}
	state.History = history
	return json.MarshalIndent(state, "", "  ")
}

func headRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return limitRunes(text, limit) + fmt.Sprintf("\n[%d more characters are in the record, not here]", utf8.RuneCountInString(text)-limit)
}

func tailRunes(text string, limit int) string {
	count := utf8.RuneCountInString(text)
	if count <= limit {
		return text
	}
	cut := 0
	for skipped := 0; skipped < count-limit; skipped++ {
		_, size := utf8.DecodeRuneInString(text[cut:])
		cut += size
	}
	return fmt.Sprintf("[%d earlier characters are in the record, not here]\n%s", count-limit, text[cut:])
}

// reportHistory collapses the record and says, per kept index, how many
// earlier repetitions each collapsed failure stands for.
func reportHistory(history []chain.Result) ([]chain.Result, map[int]int) {
	var kept []chain.Result
	counts := map[int]int{}
	run, lastNote := -1, ""
	for _, result := range history {
		failure := result.Error != "" && result.Speaker != "runtime"
		if failure && run >= 0 && kept[run].Role == result.Role && kept[run].Speaker == result.Speaker && kept[run].Error == result.Error {
			kept[run] = result
			counts[run]++
			continue
		}
		if result.Speaker == "runtime" && run >= 0 && result.Role == kept[run].Role && result.Error == "" {
			if note := result.Output; note == lastNote {
				continue
			} else {
				lastNote = note
			}
			kept = append(kept, result)
			continue
		}
		kept = append(kept, result)
		run, lastNote = -1, ""
		if failure {
			run = len(kept) - 1
		}
	}
	if len(kept) > reportRecords {
		omitted := len(kept) - reportRecords
		kept = append([]chain.Result{{Role: "runtime", Speaker: "runtime",
			Output: fmt.Sprintf("%d earlier records are in the request's history and not repeated here.", omitted)}}, kept[omitted:]...)
		shifted := map[int]int{}
		for index, count := range counts {
			if index >= omitted {
				shifted[index-omitted+1] = count
			}
		}
		counts = shifted
	}
	return kept, counts
}

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
	observations, err := stopObservations(state)
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
	// its existing recovery loop; never restart the original connections. An
	// ordered run has no connections to keep either; its report runs the one
	// role until it returns, and no decision service reads the record, whose
	// size is whatever the stopped work left behind.
	bound.Workflow = nil
	if bound.Router.Mode == "stages" {
		bound.Router.Mode = "single"
	}
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
	if err := adoptReportRequest(filepath.Join(directory, "stop-report"), request); err != nil {
		return err
	}
	fmt.Fprintf(log, "request %d: reporting the authorized stop without resuming the original work\n", issue.ID)
	return run(ctx, []string{"--config", configPath, "--request", requestPath, "--run-dir", filepath.Join(directory, "stop-report")}, io.Discard, log)
}

// adoptReportRequest lets an unfinished report continue under the request
// text this runtime renders. The text is the runtime's own rendering of the
// stopped request's record, so a runtime that renders it differently, after
// an upgrade, must not read its predecessor's report directory as another
// request's and wait forever; the record of earlier attempts is kept.
func adoptReportRequest(directory, request string) error {
	raw, err := os.ReadFile(filepath.Join(directory, "history.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved chain.State
	if err := json.Unmarshal(raw, &saved); err != nil {
		return err
	}
	if saved.Request == request || saved.Done {
		return nil
	}
	// Only a report's own directory is adopted: its text always opens with
	// the reporting instructions. Anything else is not this runtime's to
	// take; either the directory holds some other text, or the instructions
	// themselves changed with the runtime, which the same words cover.
	if !strings.HasPrefix(saved.Request, stopReportingInstructions) {
		return errors.New("the report directory's text does not open with this runtime's reporting instructions (another request's text, or instructions that changed with the runtime); not adopted")
	}
	// The rewrite happens under the directory's own lock, taken with the
	// saved text, so a runtime still writing there is not written over. While
	// that runtime holds it, this tick leaves the report alone and a later
	// tick returns to it.
	store, err := chain.Open(directory, saved.Request)
	if err != nil {
		return fmt.Errorf("the report directory is in use; adopting its request text later: %w", err)
	}
	defer store.Close()
	state, err := store.Load()
	if err != nil {
		return err
	}
	if state.Request == request || state.Done {
		return nil
	}
	state.Request = request
	return store.Save(state)
}
