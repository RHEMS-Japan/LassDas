package runner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/livelog"
)

// CurrentStepFile names the record that says which step is running right
// now. Until it existed, everything before the first work card looked
// identical from outside - a reception of a dozen steps showed one line,
// "作業の準備をしています", for its whole length, and a requester could not
// tell a run that was working from one that had died (live 2026-09-17:
// 「やってるのかやってないのかわからんな」).
const CurrentStepFile = "current-step.json"

// CurrentStep is that record: the step's name and when it started.
type CurrentStep struct {
	Step      string    `json:"step"`
	StartedAt time.Time `json:"started_at"`
}

// recordCurrentStep replaces the record as a step begins. Best-effort like
// the trail: a record that cannot be written changes nothing about the step
// it describes.
func (p *Pipeline) recordCurrentStep(name string) {
	if !UsableStepName(name) {
		return
	}
	encoded, err := json.Marshal(CurrentStep{Step: name, StartedAt: time.Now().UTC()})
	if err != nil {
		return
	}
	_ = writeRecordAtomically(p.path(CurrentStepFile), encoded)
}

// ClearCurrentStep removes the record when the run stops running steps, so a
// finished run does not keep naming the step it ended on.
func ClearCurrentStep(runDir string) {
	if err := os.Remove(currentStepPath(runDir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
}

// ReadCurrentStep reads the record. A missing, unreadable or malformed
// record is reported as absent: the caller then says what it said before.
func ReadCurrentStep(runDir string) (CurrentStep, bool) {
	raw, err := readWorkspaceFile(currentStepPath(runDir), maxCurrentStepBytes)
	if err != nil {
		return CurrentStep{}, false
	}
	var step CurrentStep
	if json.Unmarshal(raw, &step) != nil || !UsableStepName(step.Step) || step.StartedAt.IsZero() {
		return CurrentStep{}, false
	}
	return step, true
}

const maxCurrentStepBytes = 4096

func currentStepPath(runDir string) string {
	return runDir + string(os.PathSeparator) + CurrentStepFile
}

// LiveLogPath is where a step appends what it is producing while it runs.
// One file per step name, inside the run directory, so a reader can follow
// one stage without reading the whole container's output.
func LiveLogPath(runDir, step string) string {
	return filepath.Join(runDir, livelog.Dir, LiveLogName(step)+".log")
}

// LiveLogName is the step's name as a file name: a step is named for humans
// ("git checkout", "agent-review"), and the file must stay one path segment.
func LiveLogName(step string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, step)
	name = strings.Trim(name, "-.")
	if name == "" || len(name) > 80 {
		if len(name) > 80 {
			return name[:80]
		}
		return "step"
	}
	return name
}
