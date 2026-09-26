package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"automation.internal/ticket-ingress/internal/worker"
)

const acceptedReceptionFile = "accepted-reception.json"

// An accepted reception is not a model turn to repeat. Its judge may have
// settled questions, and the checked inputs alone do not contain that answer.
// Keep the exact sealed decision before using its routing. A damaged working
// copy can then be restored without questions, a changed plan, or lost cards.
// This copy belongs to this attempt: Prepare removes it on an explicit fresh
// attempt, just like the ticket and source it is bound to.
func receptionRecord(runDir, configPath string) ([]byte, error) {
	if err := receptionDirectory(runDir); err != nil {
		return nil, fmt.Errorf("%w: reception directory is unavailable: %v", ErrReadinessDecisionUnreadable, err)
	}
	path := filepath.Join(runDir, "history", "readiness", "decision.json")
	accepted := filepath.Join(runDir, acceptedReceptionFile)
	raw, readErr := worker.ReadBoundedRegularFile(path, worker.MaxReadinessJSONBytes)
	config, request, source, boundErr := receptionInputs(runDir, configPath)
	readDecision := func(filename string) (worker.ReadinessDecision, error) {
		var decision worker.ReadinessDecision
		if boundErr != nil {
			return decision, boundErr
		}
		if err := worker.ReadJSONFile(filename, worker.MaxReadinessJSONBytes, &decision); err != nil {
			return decision, err
		}
		if decision.Outcome != worker.ReadinessOutcomeReady {
			return decision, errors.New("the reception was not accepted")
		}
		return decision, decision.ValidateBinding(source, request, config)
	}
	saved, savedErr := readDecision(accepted)
	current, currentErr := readDecision(path)
	if savedErr == nil {
		if currentErr == nil && current.DecisionSHA256 == saved.DecisionSHA256 {
			return json.Marshal(current)
		}
		return restoreReception(path, saved)
	}
	if currentErr == nil {
		// Checkpoint failures do not turn a sound current decision into a
		// failed delivery. Keep its reason in the log and try on the next
		// read. No new decision is made or substituted here.
		if err := writeReceptionRecord(accepted, current); err != nil {
			fmt.Fprintf(os.Stderr, "runner: accepted reception checkpoint unavailable: %v\n", err)
		}
		return json.Marshal(current)
	}
	// Older runs may have no checkpoint. Re-derive only from complete,
	// validated assessment/check pairs, without making another model call.
	// If the old judge was needed to settle questions its answer was lost:
	// do not fabricate it or ask those questions again.
	if boundErr == nil {
		decision, err := rederiveAcceptedReception(runDir, source, request, config)
		if err == nil {
			if err := writeReceptionRecord(accepted, decision); err != nil {
				return nil, fmt.Errorf("%w: checkpoint could not be restored: %v", ErrReadinessDecisionUnreadable, err)
			}
			return restoreReception(path, decision)
		}
		return nil, fmt.Errorf("%w: saved decision: %v; current decision: %v; checked inputs: %v", ErrReadinessDecisionUnreadable, savedErr, currentErr, err)
	}
	// Preserve pre-schema routing records, but not an empty object or a
	// damaged modern run masquerading as an old record. Modern runs have
	// bound input files even when one of them has become unreadable.
	legacy := readErr == nil
	for _, name := range []string{acceptedReceptionFile, "readiness-ticket.json", "readiness-source.json"} {
		if _, err := os.Lstat(filepath.Join(runDir, name)); !errors.Is(err, os.ErrNotExist) {
			legacy = false
		}
	}
	if legacy && validLegacyReception(raw) {
		return raw, nil
	}
	return nil, fmt.Errorf("%w: bound reception records are unavailable: %v", ErrReadinessDecisionUnreadable, boundErr)
}

func receptionInputs(runDir, configPath string) (worker.Config, worker.TicketRequest, worker.SourceSnapshot, error) {
	config, err := worker.LoadConfig(configPath)
	var request worker.TicketRequest
	var source worker.SourceSnapshot
	if err == nil {
		err = worker.ReadJSONFile(filepath.Join(runDir, "readiness-ticket.json"), worker.MaxTicketJSONBytes, &request)
	}
	if err == nil {
		err = worker.ReadJSONFile(filepath.Join(runDir, "readiness-source.json"), worker.MaxArtifactJSONBytes, &source)
	}
	if err == nil {
		err = source.Validate(request, config)
	}
	return config, request, source, err
}

func rederiveAcceptedReception(runDir string, source worker.SourceSnapshot, request worker.TicketRequest, config worker.Config) (worker.ReadinessDecision, error) {
	var assessments []worker.ReadinessAssessment
	var checks []worker.ReadinessCheck
	for attempt := 1; attempt <= worker.MaxReadinessAttempts; attempt++ {
		dir := filepath.Join(runDir, "history", "readiness")
		var assessment worker.ReadinessAssessment
		var check worker.ReadinessCheck
		if err := worker.ReadJSONFile(filepath.Join(dir, fmt.Sprintf("assessment-%d.json", attempt)), worker.MaxReadinessJSONBytes, &assessment); err != nil {
			return worker.ReadinessDecision{}, fmt.Errorf("assessment %d is unavailable: %w", attempt, err)
		}
		if err := worker.ReadJSONFile(filepath.Join(dir, fmt.Sprintf("check-%d.json", attempt)), worker.MaxReadinessJSONBytes, &check); err != nil {
			return worker.ReadinessDecision{}, fmt.Errorf("check %d is unavailable: %w", attempt, err)
		}
		assessments = append(assessments, assessment)
		checks = append(checks, check)
		if check.Verdict == "pass" {
			break
		}
	}
	decision, err := worker.DecideReadiness(context.Background(), assessments, checks, source, request, config, nil)
	if err != nil {
		return decision, fmt.Errorf("checked reception could not be re-derived: %w", err)
	}
	if decision.Outcome != worker.ReadinessOutcomeReady {
		return decision, errors.New("checked inputs do not establish the accepted decision; its saved judgment is required")
	}
	return decision, nil
}

func validLegacyReception(raw []byte) bool {
	var legacy struct {
		SchemaVersion int    `json:"schema_version"`
		Outcome       string `json:"outcome"`
		RequestKind   string `json:"request_kind"`
		NeedsDesign   *bool  `json:"needs_design"`
	}
	if json.Unmarshal(raw, &legacy) != nil || legacy.SchemaVersion != 0 ||
		(legacy.Outcome != "" && legacy.Outcome != worker.ReadinessOutcomeReady) ||
		(legacy.RequestKind != "" && legacy.RequestKind != worker.RequestKindChange && legacy.RequestKind != worker.RequestKindInvestigation) {
		return false
	}
	return legacy.Outcome == worker.ReadinessOutcomeReady ||
		((legacy.RequestKind == worker.RequestKindChange || legacy.RequestKind == worker.RequestKindInvestigation) && legacy.NeedsDesign != nil)
}

// ReceptionStarted distinguishes an accepted (or damaged) handoff from a
// preparation that never reached it. An unfinished/question-bearing gate
// without an accepted copy still belongs to reception, not to this recovery.
// Presence is not permission to start a card: ChainPlanFromDecision must
// validate or restore the decision first.
func ReceptionStarted(runDir string) bool {
	if _, err := os.Lstat(filepath.Join(runDir, acceptedReceptionFile)); !errors.Is(err, os.ErrNotExist) {
		return true
	}
	path := filepath.Join(runDir, "history/readiness/decision.json")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return false
	}
	raw, err := worker.ReadBoundedRegularFile(path, worker.MaxReadinessJSONBytes)
	if err != nil {
		return true
	}
	var header struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(raw, &header) == nil {
		switch header.Outcome {
		case "clarification_required", "reject", "unresolved":
			return false
		}
	}
	return true
}

func restoreReception(path string, decision worker.ReadinessDecision) ([]byte, error) {
	if err := receptionDirectory(filepath.Dir(filepath.Dir(filepath.Dir(path)))); err != nil {
		return nil, fmt.Errorf("%w: restoration directory is unavailable: %v", ErrReadinessDecisionUnreadable, err)
	}
	// Preserve the damaged record by renaming, not copying unbounded or
	// special-file contents. A symlink is retained as a link, never followed.
	if _, err := os.Lstat(path); err == nil {
		retained, err := os.CreateTemp(filepath.Dir(path), "decision-damaged-*.json")
		if err != nil {
			return nil, fmt.Errorf("%w: could not retain damaged decision: %v", ErrReadinessDecisionUnreadable, err)
		}
		name := retained.Name()
		if err := retained.Close(); err != nil {
			_ = os.Remove(name)
			return nil, fmt.Errorf("%w: damaged decision could not be retained: %v", ErrReadinessDecisionUnreadable, err)
		}
		if err := os.Rename(path, name); err != nil {
			_ = os.Remove(name)
			if errors.Is(err, os.ErrNotExist) {
				// Another reader may already have retained the same damage.
				// Both writers restore the same validated decision atomically.
				if err := writeReceptionRecord(path, decision); err == nil {
					return json.Marshal(decision)
				}
			}
			return nil, fmt.Errorf("%w: could not retain damaged decision: %v", ErrReadinessDecisionUnreadable, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: decision could not be inspected: %v", ErrReadinessDecisionUnreadable, err)
	}
	if err := writeReceptionRecord(path, decision); err != nil {
		return nil, fmt.Errorf("%w: decision could not be restored: %v", ErrReadinessDecisionUnreadable, err)
	}
	return json.Marshal(decision)
}

// Never follow a replaced history directory while restoring an owned record.
// Missing directories can be rebuilt; symlinks and non-directories cannot.
func receptionDirectory(runDir string) error {
	for index, path := range []string{runDir, filepath.Join(runDir, "history"), filepath.Join(runDir, "history", "readiness")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) && index > 0 {
			if err = os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("reception record directory is not a real directory")
		}
	}
	return nil
}

func writeReceptionRecord(path string, decision worker.ReadinessDecision) error {
	encoded, err := json.Marshal(decision)
	if err != nil || int64(len(encoded)) > worker.MaxReadinessJSONBytes {
		return errors.New("accepted reception is too large to record")
	}
	// Distinct temporary names matter: the attendant and a running card
	// can both read the same accepted decision. Neither should see a partial
	// checkpoint or remove the other writer's temporary file.
	file, err := os.CreateTemp(filepath.Dir(path), ".reception-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := bytes.NewReader(encoded).WriteTo(file)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
