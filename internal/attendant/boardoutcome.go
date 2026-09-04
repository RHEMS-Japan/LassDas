package attendant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// The ticket is the source of truth for final outcomes, but several of
// them — expired, stopped, dead cards — exist ONLY as posted comments: no
// artifact file records them. The status board classifies from files and
// cards, so without a seal it keeps telling the previous story ("waiting
// for Go") about a run whose ending was already posted (adversarial audit
// 2026-09-01, findings 1-1..1-3). sealBoardOutcome writes the missing
// fact at the exact moment the post sticks; only the observation-only
// snapshot (status.go) reads it — nothing decides on it.

const boardOutcomeFile = "board-outcome.json"

type boardOutcome struct {
	Phase   string    `json:"phase"` // staging | release | e2e
	Verdict string    `json:"verdict"`
	Note    string    `json:"note,omitempty"`
	At      time.Time `json:"at"`
}

// sealBoardOutcome is best-effort by design: a failed seal costs board
// accuracy, never the tick or the ticket.
func sealBoardOutcome(runDir, phase, verdict, note string) {
	encoded, err := json.Marshal(boardOutcome{Phase: phase, Verdict: verdict, Note: note, At: time.Now().UTC()})
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(runDir, boardOutcomeFile), encoded, 0o644)
}

func readBoardOutcome(runDir string) (boardOutcome, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, boardOutcomeFile))
	if err != nil {
		return boardOutcome{}, false
	}
	var outcome boardOutcome
	if json.Unmarshal(raw, &outcome) != nil || outcome.Phase == "" || outcome.Verdict == "" {
		return boardOutcome{}, false
	}
	return outcome, true
}
