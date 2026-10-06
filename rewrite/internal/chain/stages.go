package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A stage is satisfied by something the runtime observed, never by what a role
// wrote. A command stage is satisfied when its configured commands all exit 0.
// A model stage is satisfied when its processes ran without a process error:
// its words are not read at all, and the command stage that follows is what
// proves the work. There is no counter and no failing end state.
const (
	ModelStage   = "model"
	CommandStage = "command"
)

// Stage is one step of an ordered run. Name is a configured role. OnFailure
// names the model stage that receives a failed command's output before the
// command runs again; a model stage takes none, because a process error simply
// launches it again.
type Stage struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	OnFailure string `json:"on_failure,omitempty"`
	// Announce is the operator's own sentence for the requester, posted once
	// when this stage first begins; empty says nothing.
	Announce string `json:"announce,omitempty"`
}

// StageRouter advances an ordered run on observed facts. It calls no model of
// its own. The single judgment left to a model is at the entrance, where the
// configured decision router chooses between the operator's question role and
// the next stage; it is never offered done, so a question cannot end a request.
type StageRouter struct {
	Entrance Router
}

func (r StageRouter) Next(ctx context.Context, state State) (Assignment, error) {
	if state.Workflow == nil || len(state.Workflow.Stages) == 0 {
		return Assignment{}, errors.New("stage progression needs a configured ordered run")
	}
	actions := state.stageActions()
	if len(actions) > 1 {
		if r.Entrance == nil {
			return Assignment{}, errors.New("asking the requester at the entrance needs a configured decision router")
		}
		next, err := r.Entrance.Next(ctx, state)
		if err != nil {
			return Assignment{}, err
		}
		// The entrance router only chooses which stage runs; what the stage
		// is told comes from the runtime, never from the model's own words.
		next.Instruction = state.stageInstruction(next.Role)
		return next, nil
	}
	if len(actions) != 1 || actions[0] == "" {
		return Assignment{}, fmt.Errorf("the ordered run has no stage after %q", state.Step)
	}
	if actions[0] == "done" {
		return Assignment{Role: "done"}, nil
	}
	return Assignment{Role: actions[0], Instruction: state.stageInstruction(actions[0])}, nil
}

// stageRun is what the runtime observed of one launch: which role ran, and
// whether every result it produced was free of a process error.
type stageRun struct {
	role      string
	satisfied bool
}

// stageRuns groups the history into the launches the runtime made. A launch is
// its role's consecutive entries up to and including the runtime's own note
// about it: the record written when a stage returns, or the note left for an
// action a crash interrupted. Without that boundary an interrupted stage and
// the stage that ran next would read as one launch, and a failure that was
// already repaired would never clear. A routing observation is the runtime's
// note about an unavailable decision, not a launch of anything.
func (s State) stageRuns() []stageRun {
	var runs []stageRun
	for i := 0; i < len(s.History); {
		role, satisfied := s.History[i].Role, true
		j := i
		for j < len(s.History) && s.History[j].Role == role {
			satisfied = satisfied && s.History[j].Error == ""
			ended := s.History[j].Speaker == "runtime"
			j++
			if ended {
				break
			}
		}
		i = j
		if role != "" && role != "router" {
			runs = append(runs, stageRun{role: role, satisfied: satisfied})
		}
	}
	return runs
}

func stageAt(stages []Stage, name string) (Stage, int) {
	for i, stage := range stages {
		if stage.Name == name {
			return stage, i
		}
	}
	return Stage{}, -1
}

// stageActions reports what may run next. Everything here comes from results
// the runtime observed; no text written by any role is read or compared.
func (s State) stageActions() []string {
	stages := s.Workflow.Stages
	satisfied, last := map[string]bool{}, stageRun{}
	for _, run := range s.stageRuns() {
		satisfied[run.role] = run.satisfied
		// A stage that runs again, as the work does after a failed command,
		// leaves the stages after it with nothing observed about the new
		// state: what they proved was the earlier state. They run again.
		if _, index := stageAt(stages, run.role); index >= 0 {
			for _, later := range stages[index+1:] {
				satisfied[later.Name] = false
			}
		}
		last = run
	}
	next := "done"
	for _, stage := range stages {
		if !satisfied[stage.Name] {
			next = stage.Name
			break
		}
	}
	stage, index := stageAt(stages, last.role)
	switch {
	case index < 0 && last.role != "":
		// The question role is the only role outside the ordered run. Whatever
		// the requester replied, the run settles the request again first.
		return []string{stages[0].Name}
	case index >= 0 && !last.satisfied && stage.Kind == CommandStage:
		// The command did not exit 0. What it returned is already in the
		// record; the stage it names carries the work, then it runs again.
		return []string{stage.OnFailure}
	case index >= 0 && !last.satisfied:
		// A model stage that could not run, or was interrupted, runs again.
		return []string{stage.Name}
	case index == 0 && s.Workflow.Question != "" && s.Workflow.Question != s.QuestionUnavailable && next != "done":
		return []string{s.Workflow.Question, next}
	}
	return []string{next}
}

// stageInstruction is the runtime's own plain text saying which stage this is
// and what will satisfy it. It is not a format any role has to answer in, and
// it grades nothing that was written before it.
func (s State) stageInstruction(name string) string {
	stages := s.Workflow.Stages
	stage, index := stageAt(stages, name)
	if index < 0 {
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Stage %d of %d in the configured run: %s.\n", index+1, len(stages), stage.Name)
	if stage.Kind == CommandStage {
		text.WriteString("The runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\n")
	} else {
		text.WriteString("Nothing you write is read, decoded or graded, and saying the work is done advances nothing. ")
		if follow, ok := followingCommand(stages, index); ok {
			fmt.Fprintf(&text, "The %s stage runs afterwards, and what its commands return is what carries this work on.\n", follow)
		} else {
			text.WriteString("A later command stage's result is what carries this work on.\n")
		}
	}
	runs := s.stageRuns()
	if len(runs) > 0 && !runs[len(runs)-1].satisfied {
		if failed, at := stageAt(stages, runs[len(runs)-1].role); at >= 0 && failed.Kind == CommandStage {
			fmt.Fprintf(&text, "The %s stage did not exit 0. What its commands returned is in the record below; this run has no failing end state, so it is tried again after you.\n", failed.Name)
		}
	}
	return text.String()
}

func followingCommand(stages []Stage, index int) (string, bool) {
	for _, stage := range stages[index+1:] {
		if stage.Kind == CommandStage {
			return stage.Name, true
		}
	}
	return "", false
}

// validateStages checks the ordered run itself. Whether a stage's role really
// launches a model is checked where the configured processes are known.
func (w *Workflow) validateStages(roles map[string]string) error {
	if len(w.Start) != 0 || len(w.After) != 0 || len(w.Recover) != 0 {
		return errors.New("workflow.stages is the whole ordered run; remove start, after and recover")
	}
	kinds := map[string]string{}
	for _, stage := range w.Stages {
		if _, configured := roles[stage.Name]; !configured || stage.Name == "done" || stage.Name == "router" {
			return fmt.Errorf("stage %q names no configured role", stage.Name)
		}
		if _, repeated := kinds[stage.Name]; repeated {
			return fmt.Errorf("stage %q appears twice in the ordered run", stage.Name)
		}
		if stage.Kind != ModelStage && stage.Kind != CommandStage {
			return fmt.Errorf("stage %q needs kind %q or %q", stage.Name, ModelStage, CommandStage)
		}
		kinds[stage.Name] = stage.Kind
	}
	for _, stage := range w.Stages {
		if stage.Kind == CommandStage && kinds[stage.OnFailure] != ModelStage {
			return fmt.Errorf("command stage %q needs on_failure naming a model stage, because a failure never ends the request", stage.Name)
		}
		if stage.Kind == ModelStage && stage.OnFailure != "" {
			return fmt.Errorf("model stage %q takes no on_failure; a process error launches it again", stage.Name)
		}
	}
	if w.Stages[len(w.Stages)-1].Kind != CommandStage {
		return errors.New("the last stage must be a command, so an observed exit status and not a model's words finishes the run")
	}
	if w.Question != "" {
		if _, configured := roles[w.Question]; !configured {
			return fmt.Errorf("the question role %q names no configured role", w.Question)
		}
		if _, staged := kinds[w.Question]; staged {
			return fmt.Errorf("the question role %q is also a stage; it can satisfy nothing", w.Question)
		}
		if w.Stages[0].Kind != ModelStage {
			return errors.New("the entrance stage must be a model stage for the requester to be asked at all")
		}
	}
	return nil
}

// stageRecord is the runtime's own note of what one launch of a stage
// returned: how each process ended, and any receipt file the operator asked it
// to read back. It is also where the launch ends, so a later launch of the
// same stage is never read as part of an earlier one. It is plain text for the
// next stage to read, not a format anything has to produce, and it says
// nothing about whether the work is any good.
func (s State) stageRecord(assignment Assignment, results []Result) (Result, bool) {
	if s.Workflow == nil || len(s.Workflow.Stages) == 0 {
		return Result{}, false
	}
	if _, staged := stageAt(s.Workflow.Stages, assignment.Role); staged < 0 {
		return Result{}, false
	}
	record := Result{Role: assignment.Role, Speaker: "runtime", Instruction: assignment.Instruction,
		StartedAt: time.Now().UTC()}
	if len(results) == 0 {
		record.Error = "The stage returned no result at all, so nothing was observed of it."
		record.FinishedAt = time.Now().UTC()
		return record, true
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Runtime record for stage %s, written by the engine from what it observed.\n", assignment.Role)
	for _, result := range results {
		name := result.Speaker
		if name == "" {
			name = assignment.Role
		}
		if result.Error == "" {
			fmt.Fprintf(&text, "Process %s exited 0.\n", name)
		} else {
			fmt.Fprintf(&text, "Process %s did not exit 0: %s\n", name, firstLine(result.Error))
		}
		if result.Receipt != "" {
			fmt.Fprintf(&text, "Receipt %s\n", result.Receipt)
		}
	}
	record.Output = text.String()
	record.FinishedAt = time.Now().UTC()
	return record, true
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")
	return line
}
