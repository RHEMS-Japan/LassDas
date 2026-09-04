package attendant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
)

// The plan notice and the stop request are the requester-facing half of the
// chain orchestration: after the readiness gate passes, the run tells the
// requester what it is about to build (a notice, not a gate), and before any
// NEW round of cards is created it honours a stop comment. In-flight cards
// are never killed and the in-flight round's card healing is deliberately
// untouched — a stop means "no further rounds", not "abandon the round".

// planArtifactMaxBytes bounds each artifact read. The artifacts are sealed
// JSON documents of a few kilobytes; anything larger is a wrong file.
const planArtifactMaxBytes = 1 << 20

// readinessAttempts mirrors the readiness gate's own bound (up to three
// assess/check attempts), newest attempt wins.
const readinessAttempts = 3

// loadPlanFacts reads the plan notice's material from the sealed run
// directory. Every part is optional: a missing or unreadable artifact just
// leaves its section out, because the notice must never block or fail the
// run it describes.
func loadPlanFacts(runDir string) hook.PlanFacts {
	facts := hook.PlanFacts{}
	var ticket struct {
		Request     string   `json:"request"`
		TargetFiles []string `json:"target_files"`
	}
	if readPlanArtifact(filepath.Join(runDir, "readiness-ticket.json"), &ticket) == nil {
		facts.Request = ticket.Request
		facts.TargetFiles = ticket.TargetFiles
	}
	if facts.Request == "" {
		var draft struct {
			Request string `json:"request"`
		}
		if readPlanArtifact(filepath.Join(runDir, "ticket-draft.json"), &draft) == nil {
			facts.Request = draft.Request
		}
	}
	var intake struct {
		Rationale string `json:"rationale"`
	}
	if readPlanArtifact(filepath.Join(runDir, "intake.json"), &intake) == nil {
		facts.Rationale = intake.Rationale
	}
	for attempt := readinessAttempts; attempt >= 1; attempt-- {
		var assessment struct {
			Assumptions []struct {
				Statement string `json:"statement"`
			} `json:"assumptions"`
		}
		path := filepath.Join(runDir, "history", "readiness", fmt.Sprintf("assessment-%d.json", attempt))
		if readPlanArtifact(path, &assessment) != nil {
			continue
		}
		for _, assumption := range assessment.Assumptions {
			if statement := strings.TrimSpace(assumption.Statement); statement != "" {
				facts.Assumptions = append(facts.Assumptions, statement)
			}
		}
		break
	}
	return facts
}

func readPlanArtifact(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > planArtifactMaxBytes {
		return errors.New("plan artifact unreadable")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return errors.New("plan artifact unreadable")
	}
	if err := json.Unmarshal(encoded, out); err != nil {
		return errors.New("plan artifact invalid")
	}
	return nil
}

// commentLister is the one Backlog read the stop check needs. The narrow
// interface is what lets the fail-closed contract be tested with a fake.
type commentLister interface {
	ListComments(ctx context.Context, issueID, minCommentID int64) ([]hook.BacklogComment, error)
}

// stopRequested reports whether the requester has posted a stop comment on
// this run's ticket. The read fails closed: a listing error postpones the
// round rather than issuing cards past an unread stop request.
func stopRequested(ctx context.Context, backlog commentLister, allowedCreatorID, issueID int64) (bool, error) {
	comments, err := backlog.ListComments(ctx, issueID, 0)
	if err != nil {
		return false, err
	}
	return containsStopComment(comments, allowedCreatorID), nil
}

// containsStopComment scans for a comment by the allowed requester whose
// first non-blank line is exactly 「停止」. Only the first content line
// decides: a comment that merely mentions the word further down stays an
// ordinary comment, and nobody but the requester can stop the run.
func containsStopComment(comments []hook.BacklogComment, allowedCreatorID int64) bool {
	for _, comment := range comments {
		if comment.UserID != allowedCreatorID {
			continue
		}
		for _, line := range strings.Split(comment.Body, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if trimmed == "停止" {
				return true
			}
			break
		}
	}
	return false
}
