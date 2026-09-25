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
	"automation.internal/ticket-ingress/internal/runner"
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
		Request string `json:"request"`
	}
	if readPlanArtifact(filepath.Join(runDir, "readiness-ticket.json"), &ticket) == nil {
		facts.Request = ticket.Request
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
	// The design decision comes from the sealed readiness decision. A run
	// whose decision predates the design stage carries no reason, and the
	// notice then says nothing about it rather than guessing.
	//
	// The decision also holds the points the gate settled itself rather
	// than ask about, when it settled any: those are written when the
	// questions are dropped, which is after the assessment below was
	// sealed, so the assessment knows nothing about them.
	var decision struct {
		NeedsDesign  bool   `json:"needs_design"`
		DesignReason string `json:"design_reason"`
		RequestKind  string `json:"request_kind"`
		Assumptions  []struct {
			Kind      string `json:"kind"`
			Statement string `json:"statement"`
		} `json:"assumptions"`
		ReceptionJudgment *struct {
			Confidence float64 `json:"confidence"`
		} `json:"reception_judgment"`
	}
	if readPlanArtifact(filepath.Join(runDir, "history", "readiness", "decision.json"), &decision) == nil {
		facts.NeedsDesign, facts.DesignReason, facts.RequestKind = decision.NeedsDesign, decision.DesignReason, decision.RequestKind
		for _, assumption := range decision.Assumptions {
			statement := strings.TrimSpace(assumption.Statement)
			if statement == "" || assumption.Kind != assumptionDecidedKind {
				continue
			}
			facts.Decided = append(facts.Decided, statement)
		}
		if decision.ReceptionJudgment != nil {
			facts.SettledConfidence = decision.ReceptionJudgment.Confidence
		}
	}
	// A reception that had to be run twice says so where the reception's
	// other assumptions are shown. It is one: what the delivery is built on
	// is what the second reception decided, and nobody was asked whether
	// the first one had decided the same (reception_again.go).
	if line := receptionAgainAssumption(runDir); line != "" {
		facts.Assumptions = append(facts.Assumptions, line)
	}
	for attempt := readinessAttempts; attempt >= 1; attempt-- {
		var assessment struct {
			Assumptions []struct {
				Kind      string `json:"kind"`
				Statement string `json:"statement"`
			} `json:"assumptions"`
		}
		path := filepath.Join(runDir, "history", "readiness", fmt.Sprintf("assessment-%d.json", attempt))
		if readPlanArtifact(path, &assessment) != nil {
			continue
		}
		for _, assumption := range assessment.Assumptions {
			statement := strings.TrimSpace(assumption.Statement)
			if statement == "" {
				continue
			}
			// A point the reception decided instead of asking about is the
			// one the requester may want back. It is told apart by the kind
			// the assessment sealed it under, and an assessment sealed
			// before that kind existed carries none, which reads as the
			// ordinary assumption it was.
			if assumption.Kind == assumptionDecidedKind {
				facts.Decided = append(facts.Decided, statement)
				continue
			}
			facts.Assumptions = append(facts.Assumptions, statement)
		}
		break
	}
	// The assessment is sealed once, before anything is built, and holds
	// only what the reception settled. Everything decided after it — a
	// deadlock ruled on, a role moved to another provider, a stand-in put
	// where a key was wanted — happens while the delivery runs and has
	// nowhere in that sealed record to go. The run keeps those in a stream
	// of their own, and the notice reads it so a requester coming back to
	// this comment mid-run sees what has been decided since it was posted.
	//
	// Which of them they may want back is decided where the closing comment
	// decides it, so the two cannot disagree about the same decision.
	for _, decision := range runner.LoadRecordedDecisions(runDir) {
		statement := strings.TrimSpace(decision.Statement)
		if statement == "" {
			continue
		}
		if decision.Decided {
			facts.Decided = append(facts.Decided, statement)
			continue
		}
		facts.Assumptions = append(facts.Assumptions, statement)
	}
	return facts
}

// assumptionDecidedKind is internal/worker's AssumptionDefensibleDefault.
// The string is repeated rather than imported, the way the design reasons
// already are: this package cannot import the worker, which imports the
// package this one writes for. A worker test pins the kind's spelling to
// this copy, so renaming it there fails there.
const assumptionDecidedKind = "defensible_default"

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
// first non-blank line is exactly 「停止」. What counts as that comment is
// one rule, kept where the answer wait can read it too, so the stop a
// requester writes means the same thing to every part of the engine that
// looks for one. Nobody but the requester can stop the run.
func containsStopComment(comments []hook.BacklogComment, allowedCreatorID int64) bool {
	for _, comment := range comments {
		if comment.UserID == allowedCreatorID && hook.IsStopComment(comment.Body) {
			return true
		}
	}
	return false
}
