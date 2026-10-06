package chain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Workflow is operator configuration, not a verdict inferred from a role's
// answer. After a process error or interruption, Recover supplies the next
// actions. The model still judges the work and chooses among those actions.
type Workflow struct {
	Start   []string            `json:"start"`
	After   map[string][]string `json:"after"`
	Recover map[string][]string `json:"recover"`
	// Stages is the other form of the same operator configuration: an ordered
	// run whose progress the runtime decides from observed results instead of
	// a model choosing among connected names. When it is set, start, after and
	// recover are unused.
	Stages []Stage `json:"stages,omitempty"`
	// Question names the role the entrance may put the requester-only points
	// to. The engine fills it from intake.question_role, so the accepted run
	// says by itself where a person may be asked; it is not a stage.
	Question string `json:"question,omitempty"`
	// EntranceReworkLimit bounds immediate re-selection of the first ordered
	// stage. Zero uses two repeats; a requester reply starts the count over.
	EntranceReworkLimit int `json:"entrance_rework_limit,omitempty"`
	// ConfirmationReworkLimit bounds how often the decision after a stage that
	// confirms the change can choose the first stage. Zero uses two; a
	// requester reply starts the count over. It is counted apart from the
	// first stage's own limit, and neither applies at the other's decision.
	ConfirmationReworkLimit int `json:"confirmation_rework_limit,omitempty"`
	// LaunchLimit caps how many times a named role may be launched for one
	// request (a reply from the requester starts the count over). A role at
	// its cap is not offered at the next decision, so a review that keeps
	// sending the work back cannot go on all night. Cap a role only where the
	// decision that sends work back to it also offers a way forward that is
	// not the delivery itself; when every connected role is at its cap they
	// all stay offered, because a cap changes what is offered and never ends
	// a request.
	LaunchLimit map[string]int `json:"launch_limit,omitempty"`
}

func (w *Workflow) Validate(roles map[string]string) error {
	if w == nil {
		return nil
	}
	if w.EntranceReworkLimit < 0 || w.EntranceReworkLimit != 0 && len(w.Stages) == 0 {
		return errors.New("workflow.entrance_rework_limit needs an ordered run and must not be negative")
	}
	if w.ConfirmationReworkLimit < 0 || w.ConfirmationReworkLimit != 0 && len(w.Stages) == 0 {
		return errors.New("workflow.confirmation_rework_limit needs an ordered run with a stage marked confirm and must not be negative")
	}
	if len(w.Stages) > 0 {
		if len(w.LaunchLimit) > 0 {
			return errors.New("workflow.launch_limit belongs to connected roles; an ordered run advances on observed results")
		}
		return w.validateStages(roles)
	}
	for role, limit := range w.LaunchLimit {
		if _, ok := roles[role]; !ok || role == "done" {
			return fmt.Errorf("workflow launch_limit names unavailable role %q", role)
		}
		if limit < 1 {
			return fmt.Errorf("workflow launch_limit for %q must be at least 1", role)
		}
		connected := false
		for _, section := range []map[string][]string{w.After, w.Recover} {
			for _, targets := range section {
				connected = connected || slices.Contains(targets, role)
			}
		}
		if !connected {
			return fmt.Errorf("workflow launch_limit names %q, which no after or recover connection leads to", role)
		}
	}
	if w.Question != "" {
		return errors.New("workflow.question belongs to an ordered run; connect the question role with after and recover instead")
	}
	check := func(source string, targets []string, completion bool) error {
		if len(targets) == 0 {
			return fmt.Errorf("workflow %s needs a next action", source)
		}
		for _, target := range targets {
			if target == "done" && completion {
				continue
			}
			if _, ok := roles[target]; !ok || target == "done" {
				return fmt.Errorf("workflow %s names unavailable action %q", source, target)
			}
			if len(w.After[target]) == 0 || len(w.Recover[target]) == 0 {
				return fmt.Errorf("workflow action %q needs after and recover connections", target)
			}
		}
		return nil
	}
	if err := check("start", w.Start, false); err != nil {
		return err
	}
	for _, section := range []struct {
		name        string
		connections map[string][]string
	}{{"after", w.After}, {"recover", w.Recover}} {
		for source, targets := range section.connections {
			if _, ok := roles[source]; !ok || source == "done" {
				return fmt.Errorf("workflow %s has unavailable source %q", section.name, source)
			}
			if len(w.After[source]) == 0 || len(w.Recover[source]) == 0 {
				return fmt.Errorf("workflow action %q needs after and recover connections", source)
			}
			if err := check(section.name+" "+source, targets, section.name == "after"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Workflow) clone() *Workflow {
	copy := &Workflow{Start: slices.Clone(w.Start), After: map[string][]string{}, Recover: map[string][]string{},
		Stages: slices.Clone(w.Stages), Question: w.Question, EntranceReworkLimit: w.EntranceReworkLimit,
		ConfirmationReworkLimit: w.ConfirmationReworkLimit}
	if w.LaunchLimit != nil {
		copy.LaunchLimit = make(map[string]int, len(w.LaunchLimit))
		for role, limit := range w.LaunchLimit {
			copy.LaunchLimit[role] = limit
		}
	}
	for role, next := range w.After {
		copy.After[role] = slices.Clone(next)
	}
	for role, next := range w.Recover {
		copy.Recover[role] = slices.Clone(next)
	}
	return copy
}

func (s State) nextActions() []string {
	if s.Workflow == nil {
		return nil
	}
	if len(s.Workflow.Stages) > 0 {
		return s.stageActions()
	}
	if s.Step == "" {
		return s.Workflow.Start
	}
	if s.Recovering {
		return s.Workflow.Recover[s.Step]
	}
	return s.Workflow.After[s.Step]
}

func (s State) permits(role string) bool {
	return s.Workflow == nil || slices.Contains(s.nextActions(), role)
}

// launches counts how many times a role was launched for this request. The
// records one launch returns sit together in the history, so a run of
// consecutive records for one role is one launch, except that a record after
// one that ended in an error is a new launch (a role that fails and recovers
// into itself is launched again each time), and the runtime's own note about
// an interrupted launch ends the run too. The runtime's notes are not
// launches, and a reply from the requester starts the count over.
func (s State) launches(role string) int {
	count, previous, failed := 0, "", false
	for _, result := range s.History {
		if result.Speaker == "requester" {
			count, previous, failed = 0, "", false
			continue
		}
		if result.Speaker == "runtime" {
			if result.Role == role {
				previous = ""
			}
			continue
		}
		if result.Role == role && (previous != role || failed) {
			count++
		}
		previous, failed = result.Role, result.Error != ""
	}
	return count
}

func (s State) atLaunchLimit(role string) bool {
	if s.Workflow == nil {
		return false
	}
	if len(s.Workflow.Stages) > 0 {
		if role != s.Workflow.Stages[0].Name {
			return false
		}
		// Each limit counts the choices made at its own decision and applies
		// there only: after a stage that confirms the change, or after the
		// first stage.
		limit, count := s.Workflow.EntranceReworkLimit, s.entranceReworks
		if s.confirmationDecision() {
			limit, count = s.Workflow.ConfirmationReworkLimit, s.confirmationReworks
		}
		if limit == 0 {
			limit = 2
		}
		return count() >= limit
	}
	limit, capped := s.Workflow.LaunchLimit[role]
	return capped && s.launches(role) >= limit
}

// offered reports whether an action may be chosen at this decision: connected
// by the workflow and, when the operator capped its launches, not yet at the
// cap. When every connected role is at its cap they all stay offered, so a cap
// never leaves a decision without a role to choose and never ends a request.
func (s State) offered(action string) bool {
	// The limit on questions that posted nothing belongs to the questions
	// after the first stage. A stage that confirms the change can always ask:
	// without the question it could only deliver.
	unavailable := s.QuestionUnavailable
	if s.confirmationDecision() {
		unavailable = ""
	}
	if unavailable != "" && action == unavailable {
		return false
	}
	if !s.permits(action) {
		return false
	}
	if action == "done" || !s.atLaunchLimit(action) {
		return true
	}
	for _, other := range s.nextActions() {
		if other != "done" && (unavailable == "" || other != unavailable) && !s.atLaunchLimit(other) {
			return false
		}
	}
	return true
}

// launchLimitNotes returns one runtime record for each connected role that
// its cap keeps out of this decision and that has no such note yet since the
// requester last answered. The note is the runtime's own plain text; nothing
// in it judges what the role wrote.
func (s State) launchLimitNotes() []Result {
	if s.Workflow == nil {
		return nil
	}
	if len(s.Workflow.Stages) > 0 {
		role := s.Workflow.Stages[0].Name
		if !s.atLaunchLimit(role) || s.offered(role) {
			return nil
		}
		limit := "entrance rework limit"
		note := fmt.Sprintf("%s reached the entrance rework limit after %d repeats; this choice stays unavailable until a new requester reply. Choose among the remaining actions without weakening the original requirements.", role, s.entranceReworks())
		if s.confirmationDecision() {
			limit = "confirmation rework limit"
			note = fmt.Sprintf("%s reached the confirmation rework limit after %d returns from the stage that confirms the change; after that stage this choice stays unavailable until a new requester reply. Ask the requester or go on to the next stage, without weakening the original requirements.", role, s.confirmationReworks())
		}
		for i := len(s.History) - 1; i >= 0; i-- {
			result := s.History[i]
			if result.Speaker == "requester" {
				break
			}
			if result.Speaker == "runtime" && result.Role == "router" && strings.Contains(result.Output, limit) {
				return nil
			}
		}
		return []Result{{Role: "router", Speaker: "runtime", FinishedAt: time.Now().UTC(), Output: note}}
	}
	if len(s.Workflow.LaunchLimit) == 0 {
		return nil
	}
	var notes []Result
	for _, role := range s.nextActions() {
		if role == "done" || !s.atLaunchLimit(role) || s.offered(role) {
			continue
		}
		noted := false
		for i := len(s.History) - 1; i >= 0 && !noted; i-- {
			if s.History[i].Speaker == "requester" {
				break
			}
			noted = s.History[i].Speaker == "runtime" && s.History[i].Role == role && strings.Contains(s.History[i].Output, "launch limit")
		}
		if noted {
			continue
		}
		notes = append(notes, Result{Role: role, Speaker: "runtime", FinishedAt: time.Now().UTC(),
			Output: fmt.Sprintf("%s has run %d times for this request, the operator's launch limit; it is not offered again for the rest of this request (a reply from the requester starts the count over). Choose among the other connected actions.", role, s.launches(role))})
	}
	return notes
}

func routingChoices(state State, roles map[string]string) (map[string]string, error) {
	choices := make(map[string]string, len(roles)+1)
	for role, purpose := range roles {
		if state.offered(role) {
			choices[role] = purpose
		}
	}
	if state.permits("done") {
		choices["done"] = "The requested result has been delivered and verified, with no original requirement outstanding."
	}
	if len(choices) == 0 {
		return nil, fmt.Errorf("no configured next action after %q (recovering=%t)", state.Step, state.Recovering)
	}
	return choices, nil
}
