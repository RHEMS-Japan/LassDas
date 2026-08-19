package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

// Hermes drives the hermes CLI — the canonical channel for every card
// transition and read (docs/RUNTIME_POD.md: no direct kanban.db access,
// ever).
type Hermes struct {
	bin     string
	board   string
	profile string
}

func NewHermes(config Config) *Hermes {
	bin := config.HermesBin
	if bin == "" {
		bin = "hermes"
	}
	return &Hermes{bin: bin, board: config.HermesBoard, profile: config.HermesProfile}
}

func (h *Hermes) run(ctx context.Context, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, h.bin, append([]string{"kanban"}, arguments...)...)
	command.Env = append(os.Environ(), "HERMES_TUI=")
	if h.board != "" {
		command.Env = append(command.Env, "HERMES_KANBAN_BOARD="+h.board)
	}
	output, err := command.Output()
	if err != nil {
		detail := ""
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			detail = strings.TrimSpace(string(exit.Stderr))
		}
		return nil, fmt.Errorf("hermes kanban %s: %w (%s)", arguments[0], err, detail)
	}
	return output, nil
}

// BoardTask is the slice of one kanban task the attendant reasons about.
type BoardTask struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ListTasks reads every card assigned to the runner profile, archived ones
// included. The kanban itself is the only authority on which card holds
// which delivery — there is deliberately no attendant-side mapping file: a
// cached map that could be lost or trail reality is exactly what made
// "no card known" indistinguishable from "no card exists".
func (h *Hermes) ListTasks(ctx context.Context) ([]BoardTask, error) {
	output, err := h.run(ctx, "list", "--assignee", h.profile, "--archived", "--json")
	if err != nil {
		return nil, err
	}
	var tasks []BoardTask
	if err := json.Unmarshal(output, &tasks); err != nil {
		return nil, fmt.Errorf("hermes kanban list returned no task array: %s", strings.TrimSpace(string(output)))
	}
	return tasks, nil
}

// CreateCard creates (idempotently, by delivery id) the card for one run.
func (h *Hermes) CreateCard(ctx context.Context, deliveryID, title, body string) (string, error) {
	output, err := h.run(ctx, "create", title,
		"--body", body,
		"--assignee", h.profile,
		"--idempotency-key", deliveryID,
		"--created-by", "lassdas-attendant",
		// A direct-command worker emits no heartbeats, so this is the ONLY
		// stall bound (fork worker-command contract). Six hours matches the
		// workflow's job budget; the retiring instance measured a model
		// stage hanging silently for four (2026-08-19).
		"--max-runtime", "21600",
		"--json",
	)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(output, &created); err != nil || created.ID == "" {
		return "", fmt.Errorf("hermes kanban create returned no task id: %s", strings.TrimSpace(string(output)))
	}
	return created.ID, nil
}

// Unblock returns a blocked card to the flow after an adopted answer. The
// attendant unblocks strictly on new information, so it resolves the block
// (clearing the same-cause recurrence counter): without --resolve the
// kanban's unblock-loop breaker routes the card to triage on the second
// protocol-legal clarification round (fork BLOCK_RECURRENCE_LIMIT=2 vs
// MaxClarificationRounds=2 — the counter reaches the limit on round two).
func (h *Hermes) Unblock(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "unblock", "--resolve", taskID)
	return err
}

// Block parks a card while a question waits. The reason is positional in
// the canonical CLI; needs_input is the kind a human answer unparks.
func (h *Hermes) Block(ctx context.Context, taskID, reason string) error {
	_, err := h.run(ctx, "block", taskID, reason, "--kind", "needs_input")
	return err
}

// Complete retires a card whose run the ATTENDANT terminated (deadline
// expiry, cancellation). Complete only accepts running/ready/blocked/review
// cards; Archive is the fallback that accepts anything.
func (h *Hermes) Complete(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "complete", taskID)
	return err
}

// Archive retires a card unconditionally and — unlike done — releases its
// idempotency key, so a fresh card can be created for the same delivery.
func (h *Hermes) Archive(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "archive", taskID)
	return err
}

// recoverClaimGrace is how long a claim may sit with no running card
// before the attendant treats the worker as dead. The supervisor
// translates a worker exit into complete/block within seconds; the grace
// only has to outlive that translation plus one dispatch hand-off.
const recoverClaimGrace = 10 * time.Minute

// SyncCards aligns kanban cards with ledger states. Each pass derives the
// delivery→card mapping from the kanban itself (ListTasks), so a run with
// no card in the listing really has none — the precondition the claim
// recovery below needs. It is idempotent and conservative: the sealed
// ledger is the truth about runs, the board is its projection for the
// dispatcher, and every transition goes through the canonical CLI.
func SyncCards(ctx context.Context, services *Services, hermes *Hermes, logger interface {
	Info(string, ...any)
	Error(string, ...any)
}) error {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return nil
	}
	tasks, err := hermes.ListTasks(ctx)
	if err != nil {
		return err
	}
	cards := map[string]BoardTask{}
	for _, task := range tasks {
		if task.IdempotencyKey == "" {
			continue
		}
		// A live card outranks an archived one for the same delivery (an
		// archived row keeps the key bytes, but the dedup ignores it).
		if existing, ok := cards[task.IdempotencyKey]; !ok ||
			(existing.Status == "archived" && task.Status != "archived") {
			cards[task.IdempotencyKey] = task
		}
	}

	for _, run := range runs {
		task, hasCard := cards[run.DeliveryID]
		if task.Status == "archived" {
			hasCard = false
		}
		switch run.State {
		case "queued":
			switch {
			case !hasCard:
				title := run.RunID
				if run.Summary != "" {
					title = fmt.Sprintf("%s: %s", run.RunID, run.Summary)
				}
				body := fmt.Sprintf(
					"Automated ticket run.\nDelivery: %s\nTicket: %s\n\nDispatched by the LassDas attendant; the assignee profile's worker.command runs the stage pipeline.",
					run.DeliveryID, run.RunID,
				)
				created, err := hermes.CreateCard(ctx, run.DeliveryID, title, body)
				if err != nil {
					logger.Error("card create failed", "run", run.RunID, "error", err.Error())
					continue
				}
				logger.Info("card created", "run", run.RunID, "task", created)
			case task.Status == "blocked":
				// A queued run with a blocked card is a resumed run: the
				// adopted answer returned it to the queue while its card
				// sits parked for that very answer.
				if err := hermes.Unblock(ctx, task.ID); err != nil {
					logger.Error("card unblock failed", "task", task.ID, "error", err.Error())
					continue
				}
				logger.Info("card unblocked", "run", run.RunID, "task", task.ID)
			case task.Status == "scheduled" || task.Status == "triage":
				// Human lanes. scheduled is an operator's deliberate
				// time-wait; triage is the kanban's forced human decision.
				// The attendant never automates through either — the run
				// waits until a person releases the card.
			case task.Status == "done":
				// A retired card cannot be re-dispatched and its dedup
				// entry blocks a recreate (only archiving is ignored by the
				// dedup) — archive it, then create fresh next pass.
				if err := hermes.Archive(ctx, task.ID); err != nil {
					logger.Error("card archive failed", "task", task.ID, "error", err.Error())
					continue
				}
				logger.Info("retired card archived; fresh card next pass", "run", run.RunID, "task", task.ID)
			}
		case "claimed", "terminal_report_pending":
			if run.State == "terminal_report_pending" && run.QuestionSealed {
				// The tick's expiry pass owns this one (it regenerates the
				// identical report and reacquires the lease itself).
				continue
			}
			if run.ClaimedAt == 0 || time.Since(time.UnixMilli(run.ClaimedAt)) < recoverClaimGrace {
				continue
			}
			if hasCard && (task.Status == "running" || task.Status == "review") {
				continue
			}
			// No living worker: the card either shows the supervisor's
			// crash translation or — because the mapping above is derived
			// from the kanban itself, not a cache — provably does not
			// exist. Hand the run back to the queue; the queued case
			// returns the card to the flow on the next pass.
			if err := services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC()); err != nil {
				logger.Error("claim recovery failed", "run", run.RunID, "error", err.Error())
				continue
			}
			logger.Info("dead claim returned to the queue", "run", run.RunID, "claimed_at", run.ClaimedAt)
		case "awaiting_answer":
			// The runner blocks its own card before exiting; this recovers
			// a card that escaped back to the dispatchable pool (a dispatch
			// of it would just die on PullClaimed).
			// The fork's block accepts running/ready only; an attendant
			// card is parentless and lands in ready when it escapes.
			if hasCard && task.Status == "ready" {
				if err := hermes.Block(ctx, task.ID, "awaiting-answer:"+run.DeliveryID); err != nil {
					logger.Error("card block failed", "task", task.ID, "error", err.Error())
					continue
				}
				logger.Info("card re-blocked for waiting answer", "run", run.RunID, "task", task.ID)
			}
		case "terminal":
			if !hasCard || task.Status == "done" {
				continue
			}
			// Only the attendant's own terminations retire their card here;
			// a runner-reported failure leaves the supervisor's blocked card
			// as the visible record of the failure (docs/RUNTIME_POD.md).
			if run.TerminalCode != string(hook.TerminalClarificationExpired) &&
				run.TerminalCode != string(hook.TerminalCancelled) {
				continue
			}
			if err := hermes.Complete(ctx, task.ID); err != nil {
				// Complete refuses some states (todo, triage); Archive
				// retires anything.
				if err := hermes.Archive(ctx, task.ID); err != nil {
					logger.Error("card retire failed", "task", task.ID, "error", err.Error())
					continue
				}
			}
			logger.Info("card retired for attendant-terminated run", "run", run.RunID, "task", task.ID, "code", run.TerminalCode)
		}
	}
	return nil
}
