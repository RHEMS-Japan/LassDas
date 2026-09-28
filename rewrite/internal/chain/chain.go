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
	Model       string    `json:"model,omitempty"`
	Output      string    `json:"output"`
	Instruction string    `json:"instruction,omitempty"`
	Diagnostics string    `json:"diagnostics,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// State is ordinary restart state. There are no signatures or model-authored
// fields to validate. Pending records a started action whose result may have
// been lost; it is not permission to repeat an external write after a crash.
type State struct {
	Request    string      `json:"request"`
	History    []Result    `json:"history"`
	Pending    *Assignment `json:"pending,omitempty"`
	Done       bool        `json:"done"`
	Workflow   *Workflow   `json:"workflow,omitempty"`
	Step       string      `json:"step,omitempty"`
	Recovering bool        `json:"recovering,omitempty"`
}

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

type Chain struct {
	Router   Router
	Executor Executor
	Store    Store
	Workflow *Workflow
	// RetryDelay spaces unavailable-router calls. It does not limit attempts
	// or turn an outage into a completed delivery.
	RetryDelay time.Duration
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
			Error:      "The process stopped while this action was pending. Available reports may be partial, and the action may have taken effect. Inspect the working tree and external state before repeating it.",
			FinishedAt: time.Now().UTC(),
		})
		state.Pending = nil
		if err := c.save(ctx, state); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		started := time.Now().UTC()
		next, err := c.Router.Next(ctx, state)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && !state.permits(next.Role) {
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
		state.Pending = &next
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
		state.History = append(state.History, results...)
		state.Pending = nil
		if err := c.save(ctx, state); err != nil {
			if ctx.Err() != nil {
				// Stop authorizes no more work, but do not discard results that
				// the stopped role already returned. Make one local write attempt
				// without the cancelled retry loop. Keep Pending so a later
				// authorized resume still warns about uncertain external effects.
				state.Pending = &next
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
