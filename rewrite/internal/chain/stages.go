package chain

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// A stage is satisfied by something the runtime observed, never by what a role
// wrote. A command stage is satisfied when its configured commands all exit 0.
// A model stage is satisfied when its processes ran without a process error:
// its words do not prove completed work. At the entrance the decision model
// reads the report before handing over; later commands prove the work.
// Entrance re-selection is bounded; process failures are not failing end states.
const (
	ModelStage   = "model"
	CommandStage = "command"
)

// EnvironmentPause is the action the decision after the first stage is offered
// when workflow.environment_pause_limit allows it: pause the request before
// any implementation for the operator to prepare the environment.
const EnvironmentPause = "pause_for_environment"

// Stage is one step of an ordered run. Name is a configured role. OnFailure
// names the model stage that receives a failed command's output before the
// command runs again; a model stage takes none, because a process error simply
// launches it again.
type Stage struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	OnFailure string `json:"on_failure,omitempty"`
	// Confirm marks a model stage whose role reads the change before it is
	// delivered. When it finishes, the decision service chooses as after the
	// first stage: the first stage, the question role or the next stage. A
	// question chosen there holds the request until the requester comments.
	Confirm bool `json:"confirm,omitempty"`
	// AlwaysAsk, on a stage that confirms the change, has the requester see
	// every change before it is delivered: after this stage the next stage
	// is offered only once the requester has replied about this change.
	AlwaysAsk bool `json:"always_ask,omitempty"`
	// Announce is the operator's own sentence for the requester, posted once
	// when this stage first begins; empty says nothing.
	Announce string `json:"announce,omitempty"`
}

// StageRouter advances an ordered run on observed facts. It calls no model of
// its own. The judgment left to a model comes after the first stage and after
// a stage that confirms the change: there the configured decision router
// chooses the first stage, the operator's question role or the next stage; it
// is never offered done. Entrance is that decision router, at both places.
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
			return Assignment{}, errors.New("a choice that can ask the requester needs a configured decision router")
		}
		next, err := r.Entrance.Next(ctx, state)
		if err != nil {
			return Assignment{}, err
		}
		// The decision router only chooses which stage runs; what the stage
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
	// from and to bound the launch's records in the history.
	from, to int
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
		if role != "" && role != "router" {
			runs = append(runs, stageRun{role: role, satisfied: satisfied, from: i, to: j})
		}
		i = j
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

// sinceReply is the state with only the history after the requester's latest
// comment: the limits on choices start over at each reply.
func (s State) sinceReply() State {
	for i := len(s.History) - 1; i >= 0; i-- {
		if s.History[i].Speaker == "requester" {
			s.History = s.History[i+1:]
			break
		}
	}
	return s
}

// Count immediate re-selection of a successful entrance, not the initial
// pass, returns from a question, or required recovery after a failed launch.
func (s State) entranceReworks() int {
	first := s.Workflow.Stages[0].Name
	count, previous := 0, stageRun{}
	for _, run := range s.sinceReply().stageRuns() {
		if run.role == first && previous.role == first && previous.satisfied {
			count++
		}
		previous = run
	}
	return count
}

// confirmationReworks counts how often the first stage ran straight after a
// successful stage that confirms the change, since the requester's latest
// comment: the returns to requirements chosen after the change was read.
func (s State) confirmationReworks() int {
	first := s.Workflow.Stages[0].Name
	count, previous := 0, stageRun{}
	for _, run := range s.sinceReply().stageRuns() {
		if stage, index := stageAt(s.Workflow.Stages, previous.role); run.role == first && index > 0 && stage.Confirm && previous.satisfied {
			count++
		}
		previous = run
	}
	return count
}

// confirmationDecision reports that the latest launch is a successful one of
// a stage that confirms the change, so the next choice is made after it.
func (s State) confirmationDecision() bool {
	if s.Workflow == nil || len(s.Workflow.Stages) == 0 {
		return false
	}
	runs := s.stageRuns()
	if len(runs) == 0 {
		return false
	}
	last := runs[len(runs)-1]
	stage, index := stageAt(s.Workflow.Stages, last.role)
	return index > 0 && stage.Confirm && last.satisfied
}

// askingStage is the stage that ran before the question role's latest
// launches, which end the record: where that question was chosen.
func (s State) askingStage() (Stage, int) {
	if s.Workflow == nil || s.Workflow.Question == "" {
		return Stage{}, -1
	}
	runs := s.stageRuns()
	i := len(runs) - 1
	if i < 0 || runs[i].role != s.Workflow.Question {
		return Stage{}, -1
	}
	for i >= 0 && runs[i].role == s.Workflow.Question {
		i--
	}
	if i < 0 {
		return Stage{}, -1
	}
	return stageAt(s.Workflow.Stages, runs[i].role)
}

// confirmingQuestion reports that the question role's latest launch, at the
// end of the record, was chosen after a stage that confirms the change.
func (s State) confirmingQuestion() bool {
	stage, index := s.askingStage()
	return index > 0 && stage.Confirm
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
		// the requester replied goes back to the stage that asked: a stage that
		// confirms the change reads it with that change; otherwise the run
		// settles the request again first.
		if asker, at := s.askingStage(); at > 0 && asker.Confirm {
			return []string{asker.Name}
		}
		return []string{stages[0].Name}
	case index >= 0 && !last.satisfied && stage.Kind == CommandStage:
		// The command did not exit 0. What it returned is already in the
		// record; the stage it names carries the work, then it runs again.
		return []string{stage.OnFailure}
	case index >= 0 && !last.satisfied:
		// A model stage that could not run, or was interrupted, runs again.
		return []string{stage.Name}
	case index == 0 && s.Workflow.Question != "" && next != "done":
		return []string{stage.Name, s.Workflow.Question, next}
	case index > 0 && stage.Confirm && s.Workflow.Question != "" && next != "done":
		// The change has been read: requirements again, the requester, or the
		// next stage, as after the first stage. A stage that asks about every
		// change offers the next stage only once the requester has replied
		// about this one.
		if stage.AlwaysAsk && !s.changeAnswered(index) {
			return []string{stages[0].Name, s.Workflow.Question}
		}
		return []string{stages[0].Name, s.Workflow.Question, next}
	case index == 0 && next != "done" && s.environmentPauseOpen():
		// Nobody can be asked: the request may pause for the operator before
		// anything is implemented, or go on to the next stage.
		return []string{EnvironmentPause, next}
	}
	return []string{next}
}

// changeAnswered reports that the requester has replied to a question put to
// them since a stage other than the one that confirms the change, at index,
// last ran: the reply is about the change as it now stands. A stage before it
// makes or settles the change again, and one after it, such as a delivery
// whose failure is sent back to this stage, leaves a change the reply was not
// about. A question is put after the first stage or after a stage that
// confirms the change, and the stage that asked runs again on the reply, so a
// reply to another stage's question is always followed by another stage.
// Only records are read, never what any of them says.
func (s State) changeAnswered(index int) bool {
	asked, answered := false, false
	for _, record := range s.History {
		if record.Role == "router" {
			continue
		}
		if record.Speaker == "requester" {
			answered = answered || asked && record.Role == s.Workflow.Question
			continue
		}
		if _, at := stageAt(s.Workflow.Stages, record.Role); at >= 0 {
			if at != index {
				asked, answered = false, false
			}
			continue
		}
		asked = asked || record.Role == s.Workflow.Question
	}
	return answered
}

// awaitingReplyAboutChange reports a decision after a stage that asks about
// every change, before the requester has replied about this change: the next
// stage is not offered there.
func (s State) awaitingReplyAboutChange() bool {
	if !s.confirmationDecision() {
		return false
	}
	runs := s.stageRuns()
	stage, index := stageAt(s.Workflow.Stages, runs[len(runs)-1].role)
	return stage.AlwaysAsk && !s.changeAnswered(index)
}

// asksEveryChange reports that the requester is to see every change: at the
// decision after a stage that confirms the change, that stage asks about
// every change; anywhere else, some such stage does.
func (s State) asksEveryChange() bool {
	if s.Workflow == nil {
		return false
	}
	if s.confirmationDecision() {
		runs := s.stageRuns()
		stage, _ := stageAt(s.Workflow.Stages, runs[len(runs)-1].role)
		return stage.AlwaysAsk
	}
	if stage, index := s.askingStage(); index > 0 && stage.Confirm {
		return stage.AlwaysAsk
	}
	return slices.ContainsFunc(s.Workflow.Stages, func(stage Stage) bool { return stage.Confirm && stage.AlwaysAsk })
}

// environmentPauseOpen reports that the decision after the first stage may
// still pause the request because the environment falls short: the operator
// allows it, no question role is configured, the first stage is a model
// stage, no later stage has run yet, and the request has paused that way
// fewer times than allowed. Once the work has been handed to a later stage,
// the pause is not offered again, even when a failure returns to the first.
func (s State) environmentPauseOpen() bool {
	w := s.Workflow
	if w == nil || len(w.Stages) == 0 || w.EnvironmentPauseLimit <= 0 || w.Question != "" || w.Stages[0].Kind != ModelStage {
		return false
	}
	for _, run := range s.stageRuns() {
		if _, index := stageAt(w.Stages, run.role); index > 0 {
			return false
		}
	}
	return s.EnvironmentPauses() < w.EnvironmentPauseLimit
}

// stageInstruction is the runtime's own plain text saying which stage this is
// and what will satisfy it. It is not a format any role has to answer in, and
// it grades nothing that was written before it.
func (s State) stageInstruction(name string) string {
	stages := s.Workflow.Stages
	stage, index := stageAt(stages, name)
	if index < 0 {
		if name != "" && name == s.Workflow.Question && s.confirmationDecision() {
			if s.asksEveryChange() {
				return everyChangeQuestionInstruction
			}
			return confirmationQuestionInstruction
		}
		return ""
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Stage %d of %d in the configured run: %s.\n", index+1, len(stages), stage.Name)
	if stage.Kind == CommandStage {
		text.WriteString("The runtime launches this stage's configured commands and records what they returned. Their exit status is the only thing that satisfies this stage.\n")
	} else if index == 0 && s.Workflow.Question != "" {
		text.WriteString("The configured decision model reads your requirements report and chooses another requirements pass, a question or the next stage. Use ordinary prose; the runtime does not grade its wording. A later command stage proves completed work.\n")
	} else if index == 0 && s.environmentPauseOpen() {
		text.WriteString(environmentPauseStageInstruction)
	} else if stage.Confirm && s.Workflow.Question != "" && stage.AlwaysAsk {
		fmt.Fprintf(&text, everyChangeStageInstruction, stages[index+1].Name)
	} else if stage.Confirm && s.Workflow.Question != "" {
		fmt.Fprintf(&text, confirmationStageInstruction, stages[index+1].Name)
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
	if ending, ok := s.LastEnding(); ok && (ending.Interrupted || ending.Role == name) {
		text.WriteString(ending.instruction())
	}
	return text.String()
}

// Ending is how the latest launch ended when it did not end cleanly, as the
// runtime observed it: never what a role wrote about itself.
type Ending struct {
	// Role is the role the launch ran.
	Role string
	// Attempt counts the launches of Role in a row, this one included, that
	// did not end cleanly, since another role ran, as an answer kept under the
	// question role's name does; a 再開, kept without a role, does not end it.
	Attempt int
	// Interrupted is a launch a restart of the runtime cut short. Forced is
	// one of those that saved nothing, so the runtime itself was killed.
	Interrupted, Forced bool
	// TimedOut is a process stopped at its configured time limit, and
	// NoModel a launch for which a process could not select a current model.
	TimedOut, NoModel bool
	// RepeatedFailures is how many times in a row one tool call failed the
	// same way when the role's harness ended the role for that, else zero.
	RepeatedFailures int
	// Activity is what the role last recorded it was running, or empty.
	Activity string
	// Kinds counts how each of the Attempt launches ended, so a count of
	// launches in a row is never said as if all of them ended like the last.
	Kinds EndingKinds
}

// EndingKinds counts launches by how they ended, each by its first kind in
// this order: a forced exit, a stop by the runtime, the time limit, no model,
// and any other process error, among them a role its harness ended.
type EndingKinds struct {
	Forced, Stopped, TimedOut, NoModel, Errors int
}

// endingOf reads how one launch ended from its records.
func endingOf(role string, records []Result) Ending {
	ending := Ending{Role: role}
	for _, record := range records {
		ending.Interrupted = ending.Interrupted || record.Interrupted
		ending.Forced = ending.Forced || record.Forced
		ending.TimedOut = ending.TimedOut || record.Speaker != "runtime" && strings.HasPrefix(record.Error, context.DeadlineExceeded.Error())
		ending.NoModel = ending.NoModel || record.Speaker != "runtime" && strings.HasPrefix(record.Error, selectionFailure)
		if record.Activity != "" {
			ending.Activity = record.Activity
		}
		ending.RepeatedFailures = max(ending.RepeatedFailures, record.RepeatedFailures)
	}
	return ending
}

// add counts one launch's ending by its first kind.
func (k *EndingKinds) add(e Ending) {
	switch {
	case e.Forced:
		k.Forced++
	case e.Interrupted:
		k.Stopped++
	case e.TimedOut:
		k.TimedOut++
	case e.NoModel:
		k.NoModel++
	default:
		k.Errors++
	}
}

// LastEnding reports how the latest launch ended, when it did not end
// cleanly. The records a launch saved before a restart and the runtime's note
// written for it afterwards are one launch, not two.
func (s State) LastEnding() (Ending, bool) {
	var launches []stageRun
	for _, run := range s.stageRuns() {
		if n := len(launches); n > 0 && launches[n-1].role == run.role && run.to-run.from == 1 {
			note := s.History[run.from]
			if note.Speaker == "runtime" && note.Interrupted && !note.Forced && launchStopped(s.History[launches[n-1].from:launches[n-1].to]) {
				launches[n-1].to, launches[n-1].satisfied = run.to, false
				continue
			}
		}
		launches = append(launches, run)
	}
	if len(launches) == 0 || launches[len(launches)-1].satisfied {
		return Ending{}, false
	}
	last := launches[len(launches)-1]
	ending := endingOf(last.role, s.History[last.from:last.to])
	for i := len(launches) - 1; i >= 0 && launches[i].role == last.role && !launches[i].satisfied; i-- {
		ending.Attempt++
		ending.Kinds.add(endingOf(last.role, s.History[launches[i].from:launches[i].to]))
	}
	return ending, true
}

// launchStopped reports a launch whose processes the runtime stopped itself.
func launchStopped(records []Result) bool {
	for _, record := range records {
		if record.Interrupted && record.Speaker != "runtime" {
			return true
		}
	}
	return false
}

// instruction is the runtime's plain account of the ending for the launch
// that follows it, so it does not walk into the same end without knowing.
func (e Ending) instruction() string {
	how := "ended with a process error, which is in the record below"
	switch {
	case e.Forced:
		how = "was cut off when the runtime itself was killed (lack of memory is one cause)"
	case e.Interrupted:
		how = "was cut off when the runtime stopped"
	case e.TimedOut:
		how = "was stopped at its time limit"
	case e.NoModel:
		how = "could not select a current model for every process"
	}
	return fmt.Sprintf("The previous launch of %s (attempt %d in a row that did not end cleanly%s) %s. %s Do not repeat what it did unchanged: suspect the cause (memory, time or wrong arguments) and change the plan.\n",
		e.Role, e.Attempt, e.Kinds.inWords(e.Attempt), how, activityOrUnknown(e.Activity))
}

// inWords names how the launches in a row ended, when there were several and
// not all ended the same way.
func (k EndingKinds) inWords(attempts int) string {
	var parts []string
	for _, kind := range []struct {
		count       int
		one, plural string
	}{{k.Forced, "forced exit", "forced exits"}, {k.Stopped, "stop by the runtime", "stops by the runtime"},
		{k.TimedOut, "time limit", "time limits"}, {k.NoModel, "launch without a model", "launches without a model"},
		{k.Errors, "process error", "process errors"}} {
		if kind.count == attempts {
			return ""
		}
		if kind.count == 1 {
			parts = append(parts, "1 "+kind.one)
		} else if kind.count > 1 {
			parts = append(parts, fmt.Sprintf("%d %s", kind.count, kind.plural))
		}
	}
	return ": " + strings.Join(parts, ", ")
}

// What the runtime tells a stage that confirms the change, and the question
// role chosen after it. These are instructions to roles in plain words, not a
// format to answer in; nothing reads or grades what the roles write.
const confirmationStageInstruction = "Read the change actually made in the checkout before it is delivered: what Git lists as changed, the diff, new files, and commits the integration branch does not have. Read the settled requirements and the project's knowledge. Say in ordinary prose whether the change alters how a person operates the product, what a screen shows or does, or the public API. The project's knowledge defines the public API; where it does not, take the entry points used from outside: HTTP routes, command arguments and options and the output other programs read, exported functions and types, and configuration keys and file formats that others read. Name the material you read and what you could not read. A change too long to read whole is not an internal one: say what you could not read. When you cannot tell, say so. When none of the three changes, say that the requester's confirmation is not needed and why, in a line such as 依頼者の確認: なし. A requester's reply in the record covers only the change it was given about. The configured decision model reads your report and chooses the first stage again, a question to the requester, or the next stage (%s); a question holds the request until the requester comments. Use ordinary prose; the runtime does not grade its wording.\n"

const confirmationQuestionInstruction = "The decision after the stage that confirms the change chose to ask the requester before the change is delivered. Post one comment that shows the requester what the change does to how they operate the product, to its screens or to its public API, as that stage's report and the change itself show it, and ask them to choose: to deliver it as it is, to name what to change, or not to deliver it. Nothing is delivered before their reply; say so. Ask about this change only. Whatever this launch posts, the request then waits for the requester's comment.\n"

// What the runtime tells a stage that asks about every change, and the
// question chosen after it, in place of the two above: every change is shown
// to the requester, whatever it alters, and nothing says no confirmation is
// needed.
const everyChangeStageInstruction = "Read the change actually made in the checkout before it is delivered: what Git lists as changed, the diff, new files, and commits the integration branch does not have. Read the settled requirements and the project's knowledge. Say in ordinary prose what the change does: whether it alters how a person operates the product, what a screen shows or does, or the public API, and what it does when it alters none of them. Name the material you read and what you could not read; when you cannot tell, say so. In this run the requester sees every change before it is delivered, so do not say that their confirmation is not needed: say what the question to them should show. A requester's reply in the record covers only the change it was given about. The configured decision model reads your report and chooses the first stage again or a question to the requester; the next stage (%s) is offered only after the requester has replied about this same change. A question holds the request until the requester comments. Use ordinary prose; the runtime does not grade its wording.\n"

const everyChangeQuestionInstruction = "The decision after the stage that confirms the change chose to ask the requester before the change is delivered; in this run the requester sees every change, whatever it alters. Post one comment that shows the requester what the change does, as that stage's report and the change itself show it: where the runtime gives an address at which the change can be seen, point the requester to it; otherwise describe the change in words. Ask them to choose: to deliver it as it is, to name what to change, or not to deliver it. Nothing is delivered before their reply; say so. Ask about this change only. Whatever this launch posts, the request then waits for the requester's comment.\n"

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
	if w.EnvironmentPauseLimit > 0 {
		switch {
		case w.Question != "":
			return fmt.Errorf("workflow.environment_pause_limit is for an ordered run without a question role; with %q, elicitation asks the requester instead", w.Question)
		case w.Stages[0].Kind != ModelStage:
			return errors.New("workflow.environment_pause_limit needs a first stage that is a model stage, whose report the decision reads")
		}
		if _, named := roles[EnvironmentPause]; named {
			return fmt.Errorf("workflow.environment_pause_limit offers the decision %q, which is also the name of a configured role", EnvironmentPause)
		}
	}
	confirms := false
	for index, stage := range w.Stages {
		if stage.AlwaysAsk && !stage.Confirm {
			return fmt.Errorf("stage %q has always_ask, which belongs to a stage marked confirm: the requester is asked about the change after that stage", stage.Name)
		}
		if !stage.Confirm {
			continue
		}
		confirms = true
		switch {
		case stage.Kind != ModelStage:
			return fmt.Errorf("stage %q confirms the change, so it must be a model stage whose role reads the change", stage.Name)
		case index == 0:
			return fmt.Errorf("stage %q is the first stage, which a choice already follows; confirm marks a later model stage", stage.Name)
		case w.Question == "":
			return fmt.Errorf("stage %q confirms the change, but no question role is configured (intake.question_role), so the choice after it could only deliver", stage.Name)
		}
		// The requester's reply is about the change this stage read. A model
		// stage right after it could change the checkout after that reply,
		// and nothing would ask about what it changed.
		if stage.AlwaysAsk && index+1 < len(w.Stages) && w.Stages[index+1].Kind == ModelStage {
			return fmt.Errorf("stage %q has always_ask, but stage %q right after it is a model stage: what it changed after the requester's reply would be delivered without being asked about; put a command stage, such as the delivery, right after it", stage.Name, w.Stages[index+1].Name)
		}
		// The same holds for the stage the delivery right after it sends a
		// failure to: only a stage before this one has its change asked about.
		if stage.AlwaysAsk && index+1 < len(w.Stages) && w.Stages[index+1].Kind == CommandStage {
			after := w.Stages[index+1]
			if _, at := stageAt(w.Stages, after.OnFailure); at >= index {
				return fmt.Errorf("stage %q has always_ask, but stage %q right after it sends a failure (on_failure) to stage %q, which is not before it: what that stage changed would be delivered without being asked about; name a stage before %q in on_failure", stage.Name, after.Name, after.OnFailure, stage.Name)
			}
		}
	}
	if w.ConfirmationReworkLimit != 0 && !confirms {
		return errors.New("workflow.confirmation_rework_limit needs a stage marked confirm")
	}
	if w.Preview != nil {
		return w.validatePreview(roles, kinds, confirms)
	}
	return nil
}

// validatePreview checks the operator's preview roles: two different
// configured roles, neither a stage nor the question role, in a run whose
// stage that confirms the change is what comes before the question they put
// the page up for.
func (w *Workflow) validatePreview(roles map[string]string, stages map[string]string, confirms bool) error {
	if !confirms {
		return errors.New("workflow.preview needs a stage marked confirm: the page is put up only before the question chosen after that stage")
	}
	for _, role := range []struct{ setting, name string }{{"workflow.preview.up", w.Preview.Up}, {"workflow.preview.down", w.Preview.Down}} {
		if role.name == "" {
			return fmt.Errorf("%s needs a configured role", role.setting)
		}
		if _, configured := roles[role.name]; !configured || role.name == "done" || role.name == "router" {
			return fmt.Errorf("%s names no configured role: %q", role.setting, role.name)
		}
		if _, staged := stages[role.name]; staged {
			return fmt.Errorf("%s names %q, which is a stage of the ordered run; the preview roles run outside it", role.setting, role.name)
		}
		if role.name == w.Question {
			return fmt.Errorf("%s names %q, which is the question role", role.setting, role.name)
		}
	}
	if w.Preview.Up == w.Preview.Down {
		return fmt.Errorf("workflow.preview.up and workflow.preview.down must name different roles, not both %q", w.Preview.Up)
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
	if s.Workflow == nil {
		return Result{}, false
	}
	_, staged := stageAt(s.Workflow.Stages, assignment.Role)
	if staged < 0 && (len(s.Workflow.Stages) > 0 || s.Workflow.LaunchLimit[assignment.Role] == 0) {
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
	kind := "stage"
	if staged < 0 {
		kind = "role"
	}
	fmt.Fprintf(&text, "Runtime record for %s %s, written by the engine from what it observed.\n", kind, assignment.Role)
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

// EnvironmentPauses is how many times the decision after the first stage
// paused this request because the environment falls short, as the runtime's
// own records of those pauses in the history say. A resume does not take any
// away.
func (s State) EnvironmentPauses() int {
	count := 0
	for _, record := range s.History {
		if environmentPauseRecord(record) {
			count++
		}
	}
	return count
}

// EnvironmentPaused reports a request whose latest record is that pause: no
// words have resumed it since.
func (s State) EnvironmentPaused() bool {
	return len(s.History) > 0 && environmentPauseRecord(s.History[len(s.History)-1])
}

func environmentPauseRecord(record Result) bool {
	return record.Role == "router" && record.Speaker == "runtime" && record.Error == "" && record.Output == environmentPauseNote
}

// What the first stage is told while the decision after it can pause the
// request, and what the decision is told that pause means. These are plain
// words for a model, not a format to answer in.
const environmentPauseStageInstruction = "The configured decision model reads your requirements report and chooses to pause this request before anything is implemented, for the operator to enlarge or prepare the execution environment, or the next stage. When you find the environment falls short, write the missing or insufficient fact and what the operator would have to prepare in one or two sentences, in a paragraph of their own before anything else in the report: if the request pauses, the requester is shown the report from that paragraph on, up to 200 characters. Use ordinary prose; the runtime does not grade its wording. A later command stage proves completed work.\n"

const environmentPauseChoice = "Pause this request before anything is implemented, because a fact in the requirements report already shows the execution environment falls short of what the agreed verification will build and run. Nothing runs while it is paused; the requester is told what the report says is missing and what the operator would have to prepare, and after the operator enlarges or prepares the environment, 再開 continues this same request from this decision. Choose it only then: when only the demand is unknown, choose the next stage, where the heaviest part of the verification runs once first."

// environmentPauseNote is the runtime's record that the decision after the
// first stage paused the request. It is the runtime's own text, and the record
// that holds the request until words that resume it follow.
const environmentPauseNote = "The decision after the first stage paused this request before anything was implemented, because a fact shows the execution environment falls short of the agreed verification. Nothing runs until 再開 is written, after the operator has enlarged or prepared the environment."
