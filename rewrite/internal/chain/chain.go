// Package chain passes the request and the roles' own words between
// configured roles. Only the decision model chooses the next role. A role's
// output is never decoded as a candidate, review, verdict or completion mark.
package chain

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Result records what ran, not whether its answer deserves to proceed.
type Result struct {
	Role    string `json:"role"`
	Speaker string `json:"speaker"`
	// Model is the endpoint requested for this process, not a quality mark or
	// proof of which provider ultimately served the request.
	Model string `json:"model,omitempty"`
	// ModelPrefix is the gateway prefix prepended to Model when this process
	// was invoked, so the history shows which account the call was billed to.
	// It is a route to the same model, not a different model or a quality mark.
	ModelPrefix string `json:"model_prefix,omitempty"`
	Output      string `json:"output"`
	Instruction string `json:"instruction,omitempty"`
	// Receipt carries the content of the file the operator named for this
	// process, read back by the runtime after the process returned. It reaches
	// the stage's runtime record; it is not a field of the saved history and
	// nothing here interprets what it says.
	Receipt     string `json:"-"`
	Diagnostics string `json:"diagnostics,omitempty"`
	Error       string `json:"error,omitempty"`
	// Interrupted is the controller stopping this invocation, not a process
	// failure or a successful result. Partial output and the reason stay intact.
	Interrupted bool      `json:"interrupted,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// State is ordinary restart state. There are no signatures or model-authored
// fields to validate. Pending records a started action whose result may have
// been lost; it is not permission to repeat an external write after a crash.
type State struct {
	Request  string      `json:"request"`
	History  []Result    `json:"history"`
	Pending  *Assignment `json:"pending,omitempty"`
	Done     bool        `json:"done"`
	Workflow *Workflow   `json:"workflow,omitempty"`
	Step     string      `json:"step,omitempty"`
	// Waiting records that the configured question role has handed the request
	// to a person. It is not a failure, a completion, or a judgment about any
	// answer; only the caller that owns the conversation can clear it.
	Waiting    bool `json:"waiting,omitempty"`
	Recovering bool `json:"recovering,omitempty"`
	// WaitingWithoutQuestion records that the request waits for the requester
	// although no question was seen posted: the question chosen after a stage
	// that confirms the change posted nothing, did not exit 0 or was cut short
	// by a restart. The requester's comment clears it together with Waiting.
	WaitingWithoutQuestion bool `json:"waiting_without_question,omitempty"`
	// These count successful launches observed to have posted no question.
	// A new requester comment clears the count, not a role's claim of a reply.
	QuestionsWithoutPost int    `json:"questions_without_post,omitempty"`
	QuestionUnavailable  string `json:"question_unavailable,omitempty"`
	QuestionReplyAfter   int64  `json:"question_reply_after,omitempty"`
	// PendingSince is when the pending action's launch began. A launch cut
	// by a restart leaves no record of its own, and the note written for it
	// afterwards says when it began from this; a state saved before there
	// was this field has none, and the note has no start either.
	PendingSince time.Time `json:"pending_since,omitzero"`
}

// ErrWaiting reports that the request is held for a person's reply. The chain
// knows nothing about where that reply arrives or what it has to say.
var ErrWaiting = errors.New("the request is waiting for an answer to the question it asked")

// Assignment is a dispatch instruction, not a certificate of output quality.
type Assignment struct {
	Role        string `json:"role"`
	Instruction string `json:"instruction,omitempty"`
}

type Router interface {
	Next(context.Context, State) (Assignment, error)
}

type Executor interface {
	Execute(context.Context, Assignment, State) []Result
}

type Store interface {
	Load() (State, error)
	Save(State) error
}

type QuestionObservation struct {
	Posted      bool
	LastComment int64
}

type Chain struct {
	Router   Router
	Executor Executor
	Store    Store
	Workflow *Workflow
	// RetryDelay spaces unavailable routing and question reads. It does not limit attempts
	// or turn an outage into a completed delivery.
	RetryDelay time.Duration
	// WaitAfter names the configured role that may hand the request to a
	// person after QuestionPosted confirms its post. The chain reports it; the
	// caller decides what counts as a reply and appends it to the history.
	WaitAfter string
	// QuestionPosted checks the external conversation, not the role's prose.
	// An unavailable read holds routing until it succeeds or work is paused.
	QuestionPosted      func(context.Context) (QuestionObservation, error)
	QuestionNoPostLimit int
	// RequesterReply reads native conversation metadata while the question
	// role is unavailable. An empty string means no new authorized reply.
	RequesterReply func(context.Context, int64) (string, error)
	// Observe shows progress/errors without making the observer a judge.
	Observe func(string)
}

// Run keeps handing work on until the router selects done or the caller
// stops it. Role errors, empty answers and unchanged trees are input to the
// next decision, never terminal categories invented by the runner.
func (c Chain) Run(ctx context.Context) error {
	if c.Router == nil || c.Executor == nil || c.Store == nil {
		return errors.New("role chain needs a router, executor and store")
	}
	var state State
	for {
		var err error
		state, err = c.Store.Load()
		if err == nil {
			break
		}
		c.observe("loading request history: " + err.Error())
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
	if state.Done {
		return nil
	}
	if state.Waiting {
		return ErrWaiting
	}
	if state.Workflow == nil && c.Workflow != nil {
		if len(state.History) != 0 || state.Pending != nil || state.Step != "" {
			return errors.New("cannot attach new workflow connections to an already started free-routing request")
		}
		state.Workflow = c.Workflow.clone()
		if err := c.save(ctx, state); err != nil {
			return err
		}
	}
	if state.Pending != nil {
		state.Step, state.Recovering = state.Pending.Role, true
		state.History = append(state.History, Result{
			Role: state.Pending.Role, Instruction: state.Pending.Instruction, Speaker: "runtime",
			Interrupted: true,
			Error:       "The process stopped while this action was pending. Available reports may be partial, and the action may have taken effect. Inspect the working tree and external state before repeating it.",
			StartedAt:   state.PendingSince, FinishedAt: time.Now().UTC(),
		})
		state.Pending, state.PendingSince = nil, time.Time{}
		if err := c.save(ctx, state); err != nil {
			return err
		}
	}
	// A question chosen after a stage that confirms the change is waited on
	// whatever its launch did; one chosen after the first stage is looked up
	// only when its launch returned without an error.
	confirming := state.confirmingQuestion()
	checkQuestion := c.WaitAfter != "" && state.Step == c.WaitAfter && (!state.Recovering || confirming)
	if len(state.History) > 0 {
		last := state.History[len(state.History)-1]
		checkQuestion = checkQuestion && last.Role == c.WaitAfter && last.Speaker != "requester"
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if checkQuestion {
			// A launch that did not exit 0, or that a restart cut short, is
			// not looked up: only a confirmation question is checked after one,
			// and it waits whatever was posted.
			question := QuestionObservation{}
			if !state.Recovering {
				question.Posted = true
				if c.QuestionPosted != nil {
					var err error
					question, err = c.QuestionPosted(ctx)
					if err != nil {
						c.observe("checking whether the question was posted: " + err.Error())
						if err := c.wait(ctx); err != nil {
							return err
						}
						continue
					}
				}
			}
			checkQuestion = false
			if question.Posted {
				state.Waiting = true
				if err := c.save(ctx, state); err != nil {
					return err
				}
				c.observe("waiting for an answer to " + c.WaitAfter)
				return ErrWaiting
			}
			if confirming {
				// The decision chose to show the requester the change before it
				// is delivered. Nothing this launch did or failed to post stands
				// in for their words, so the request waits for them all the same.
				// It is not a question that posted nothing after the first stage
				// and is not counted toward that limit.
				state.Waiting, state.WaitingWithoutQuestion = true, true
				message := "納品前の確認の質問は、投稿を確かめられなかった。依頼者の返答があるまで納品へは進まない"
				state.History = append(state.History, Result{Role: "router", Speaker: "runtime", Output: message, FinishedAt: time.Now().UTC()})
				if err := c.save(ctx, state); err != nil {
					return err
				}
				c.observe(message)
				return ErrWaiting
			}
			message := "質問役は質問を投稿しなかったので、そのまま進める"
			state.QuestionsWithoutPost++
			limit := c.QuestionNoPostLimit
			if limit <= 0 {
				limit = 2
			}
			if state.QuestionsWithoutPost >= limit {
				state.QuestionUnavailable = c.WaitAfter
				state.QuestionReplyAfter = question.LastComment
				message = fmt.Sprintf("質問役を %d 回起動したが質問は無かった。次の依頼者のコメントまで質問役は使わない", state.QuestionsWithoutPost)
			}
			state.History = append(state.History, Result{Role: "router", Speaker: "runtime", Output: message, FinishedAt: time.Now().UTC()})
			if err := c.save(ctx, state); err != nil {
				return err
			}
			c.observe(message)
		}
		if state.QuestionUnavailable != "" && c.RequesterReply != nil {
			answer, err := c.RequesterReply(ctx, state.QuestionReplyAfter)
			if err != nil {
				c.observe("checking for a new requester comment: " + err.Error())
			} else if answer != "" {
				state.History = append(state.History, Result{Role: c.WaitAfter, Speaker: "requester", Output: answer, FinishedAt: time.Now().UTC()})
				state.QuestionsWithoutPost, state.QuestionUnavailable, state.QuestionReplyAfter = 0, "", 0
				if err := c.save(ctx, state); err != nil {
					return err
				}
			}
		}
		started := time.Now().UTC()
		if notes := state.launchLimitNotes(); len(notes) > 0 {
			state.History = append(state.History, notes...)
			if err := c.save(ctx, state); err != nil {
				return err
			}
		}
		next, err := c.Router.Next(ctx, state)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && !state.offered(next.Role) {
			err = fmt.Errorf("action %q is not connected after %q (recovering=%t); available: %v", next.Role, state.Step, state.Recovering, state.nextActions())
		}
		if err != nil {
			c.observe("routing unavailable: " + err.Error())
			// Logging alone leaves the next model with the same uninformed
			// input. Retain the failed invocation as an observation, just as
			// for working processes, without inventing a completion verdict.
			state.History = append(state.History, Result{
				Role: "router", Speaker: "runtime", Error: err.Error(),
				StartedAt: started, FinishedAt: time.Now().UTC(),
			})
			if err := c.save(ctx, state); err != nil {
				return err
			}
			if err := c.wait(ctx); err != nil {
				return err
			}
			continue
		}
		if next.Role == "done" {
			state.Done = true
			return c.save(ctx, state)
		}
		// Where the question was chosen is taken before the launch, so a
		// launch that returns no result still waits as a confirmation.
		asking := next.Role == c.WaitAfter && state.confirmationDecision()
		launched := time.Now().UTC()
		state.Pending, state.PendingSince = &next, launched
		if err := c.save(ctx, state); err != nil {
			return err
		}
		c.observe("running " + next.Role)
		results := c.Executor.Execute(ctx, next, state)
		state.Step, state.Recovering = next.Role, len(results) == 0
		for i := range results {
			results[i].Instruction = next.Instruction
			state.Recovering = state.Recovering || results[i].Error != ""
		}
		// Ordered stages and capped connected roles get a runtime record of
		// what this launch returned, so repeated launches stay distinguishable.
		if record, staged := state.stageRecord(next, results); staged {
			results = append(results, record)
		}
		if asking && len(results) == 0 {
			// The question role is not a stage and gets no record of its own,
			// so a launch that returned nothing would leave no trace that the
			// question was chosen, and a restart would ask the decision again.
			results = append(results, Result{Role: next.Role, Speaker: "runtime", Instruction: next.Instruction,
				Error: "The question's launch returned no result at all.", StartedAt: launched, FinishedAt: time.Now().UTC()})
		}
		paced := failedFast(results) && failedFastBefore(state.History, next.Role)
		state.History = append(state.History, results...)
		state.Pending, state.PendingSince = nil, time.Time{}
		if err := c.save(ctx, state); err != nil {
			if ctx.Err() != nil {
				// Stop authorizes no more work, but do not discard results that
				// the stopped role already returned. Make one local write attempt
				// without the cancelled retry loop. Keep Pending so a later
				// authorized resume still warns about uncertain external effects.
				state.Pending, state.PendingSince = &next, launched
				if saveErr := c.Store.Save(state); saveErr != nil {
					cause := fmt.Errorf("retaining stopped role results: %w", saveErr)
					c.observe(cause.Error())
					return errors.Join(err, cause)
				}
				return ctx.Err()
			}
			return fmt.Errorf("saving role result: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if paced {
			// A role that could not even start would be launched again at
			// once and fill the record with the same failure; space the
			// attempts as router outages are spaced.
			c.observe("the launch of " + next.Role + " failed within seconds; waiting before the next attempt")
			if err := c.wait(ctx); err != nil {
				return err
			}
		}
		// A successful command may have posted nothing. Check before routing;
		// a pause during the check resumes here from the saved step above.
		confirming = asking || next.Role == c.WaitAfter && state.confirmingQuestion()
		checkQuestion = c.WaitAfter != "" && next.Role == c.WaitAfter && (!state.Recovering || confirming)
	}
}

// Retry writing the result already in memory, not the completed external
// operation. A temporary disk failure must not re-run a deployment or report.
func (c Chain) save(ctx context.Context, state State) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.Store.Save(state); err == nil {
			return nil
		} else {
			c.observe("saving request history: " + err.Error())
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c Chain) observe(message string) {
	if c.Observe != nil {
		c.Observe(message)
	}
}

func (c Chain) wait(ctx context.Context) error {
	delay := c.RetryDelay
	if delay <= 0 {
		delay = 10 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// failedFast reports a launch that ended in error within seconds of starting.
func failedFast(results []Result) bool {
	for _, result := range results {
		if result.Error != "" && result.Speaker != "runtime" && result.FinishedAt.Sub(result.StartedAt) < 10*time.Second {
			return true
		}
	}
	return false
}

// failedFastBefore reports that the previous launch of the same role, the
// latest process record in the history, also failed within seconds. One
// quick failure is retried at once, as an outage another role repairs would
// be; the second in a row is spaced, so a role that cannot start does not
// fill the record.
func failedFastBefore(history []Result, role string) bool {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Speaker == "runtime" {
			continue
		}
		return history[i].Role == role && failedFast(history[i:i+1])
	}
	return false
}
