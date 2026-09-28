package chain

import (
	"errors"
	"fmt"
	"slices"
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
}

func (w *Workflow) Validate(roles map[string]string) error {
	if w == nil {
		return nil
	}
	if len(w.Stages) > 0 {
		return w.validateStages(roles)
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
		Stages: slices.Clone(w.Stages), Question: w.Question}
	for role, next := range w.After {
		copy.After[role] = slices.Clone(next)
	}
	for role, next := range w.Recover {
		copy.Recover[role] = slices.Clone(next)
	}
	return copy
}

func (s State) nextActions() []string {
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

func routingChoices(state State, roles map[string]string) (map[string]string, error) {
	choices := make(map[string]string, len(roles)+1)
	for role, purpose := range roles {
		if state.permits(role) {
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
