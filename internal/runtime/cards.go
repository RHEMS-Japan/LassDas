package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CardLedger remembers which Hermes card belongs to which delivery. It is
// attendant-local bookkeeping (a JSON file next to the ledger), not part of
// the sealed ledger: losing it means a card might be created twice, and the
// idempotency key on creation is what makes even that harmless.
type CardLedger struct {
	path  string
	Cards map[string]string `json:"cards"` // delivery_id -> task id
}

func LoadCardLedger(ledgerPath string) (*CardLedger, error) {
	path := filepath.Join(filepath.Dir(ledgerPath), "cards.json")
	ledger := &CardLedger{path: path, Cards: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, ledger); err != nil {
		// A corrupt map must not crash-loop the resident: quarantine it and
		// start empty — creation is idempotent by delivery id, so the worst
		// outcome is one redundant (deduplicated) create per live run.
		quarantine := path + ".corrupt"
		if renameErr := os.Rename(path, quarantine); renameErr != nil {
			return nil, fmt.Errorf("cards.json is corrupt and could not be quarantined: %w", renameErr)
		}
		return &CardLedger{path: path, Cards: map[string]string{}}, nil
	}
	if ledger.Cards == nil {
		ledger.Cards = map[string]string{}
	}
	ledger.path = path
	return ledger, nil
}

// save writes atomically (temp + rename) so a crash mid-write can never
// leave a truncated file behind.
func (l *CardLedger) save() error {
	encoded, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	temporary := l.path + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, l.path)
}

// Hermes drives the hermes CLI — the canonical channel for every card
// transition (§3.1 of the runtime design: no direct kanban.db writes, ever).
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

// CreateCard creates (idempotently) the card for one delivery and returns
// its task id.
func (h *Hermes) CreateCard(ctx context.Context, deliveryID, title, body string) (string, error) {
	output, err := h.run(ctx, "create", title,
		"--body", body,
		"--assignee", h.profile,
		"--idempotency-key", deliveryID,
		"--created-by", "lassdas-attendant",
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

// Unblock returns a blocked card to the flow (the answer was adopted).
func (h *Hermes) Unblock(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "unblock", taskID)
	return err
}

// Block parks a card while a question waits. The reason is positional in
// the canonical CLI, and the kind is needs_input — a human answer is what
// unparks it. The kanban's unblock-loop breaker routes a card to triage
// after BLOCK_RECURRENCE_LIMIT (2) same-kind re-blocks after unblock;
// MaxClarificationRounds is 2, so a run blocks at most twice and the
// breaker is never reached — raising the round contract past the breaker
// would strand round-3 cards in triage (docs/RUNTIME_POD.md).
func (h *Hermes) Block(ctx context.Context, taskID, reason string) error {
	_, err := h.run(ctx, "block", taskID, reason, "--kind", "needs_input")
	return err
}

// Complete retires a card whose run reached a terminal state through the
// attendant (deadline expiry, cancellation) rather than through a runner
// exit the supervisor could translate.
func (h *Hermes) Complete(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "complete", taskID)
	return err
}

// TaskStatus reads one card's status via the CLI.
func (h *Hermes) TaskStatus(ctx context.Context, taskID string) (string, error) {
	output, err := h.run(ctx, "show", taskID, "--json")
	if err != nil {
		return "", err
	}
	var task struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(output, &task); err != nil {
		return "", fmt.Errorf("hermes kanban show returned no status: %s", strings.TrimSpace(string(output)))
	}
	return task.Status, nil
}

// recoverClaimGrace is how long a claim may sit with no running card
// before the attendant treats the worker as dead. The supervisor
// translates a worker exit into complete/block within seconds; the grace
// only has to outlive that translation plus one dispatch hand-off.
const recoverClaimGrace = 10 * time.Minute

// SyncCards aligns Hermes cards with ledger states. It is deliberately
// idempotent and conservative: the sealed ledger is the truth about runs,
// the kanban board is its projection for the dispatcher, and every card
// transition goes through the canonical CLI.
func SyncCards(ctx context.Context, services *Services, cards *CardLedger, hermes *Hermes, logger interface {
	Info(string, ...any)
	Error(string, ...any)
}) error {
	runs, err := services.Store.ScanRuns(ctx)
	if err != nil {
		return err
	}
	changed := false
	for _, run := range runs {
		taskID := cards.Cards[run.DeliveryID]
		switch run.State {
		case "queued":
			if taskID == "" {
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
				cards.Cards[run.DeliveryID] = created
				changed = true
				logger.Info("card created", "run", run.RunID, "task", created)
				continue
			}
			status, err := hermes.TaskStatus(ctx, taskID)
			if err != nil {
				logger.Error("card status failed", "task", taskID, "error", err.Error())
				continue
			}
			switch status {
			case "blocked", "scheduled":
				// A queued run with a parked card is a resumed run: the
				// adopted answer returned it to the queue while its card
				// sits blocked.
				if err := hermes.Unblock(ctx, taskID); err != nil {
					logger.Error("card unblock failed", "task", taskID, "error", err.Error())
					continue
				}
				logger.Info("card unblocked", "run", run.RunID, "task", taskID)
			case "done", "archived":
				// The card was retired while the ledger still queues the run
				// (e.g. the runner's own block failed and its exit-0 completed
				// the card before the fix that turned that into exit-1). A
				// done card is never re-dispatched, so the run gets a fresh
				// card; creation is deduplicated by delivery id + kanban
				// idempotency, and a retired card does not hold the key.
				delete(cards.Cards, run.DeliveryID)
				changed = true
				logger.Error("card was retired under a queued run; recreating", "run", run.RunID, "task", taskID)
			}
		case "claimed", "terminal_report_pending":
			// A run in these states whose card is not running has no living
			// worker: the supervisor translated the worker's death into
			// block (crash) or the card escaped entirely. After the grace,
			// hand the run back to the queue (the store refuses the
			// report_pending flavors that belong to the tick); the queued
			// case above then returns the card to the flow on the next pass.
			if run.ClaimedAt == 0 || time.Since(time.UnixMilli(run.ClaimedAt)) < recoverClaimGrace {
				continue
			}
			if run.State == "terminal_report_pending" && run.QuestionSealed {
				// The tick's expiry pass owns this one (it regenerates the
				// identical report and reacquires the lease itself).
				continue
			}
			if taskID != "" {
				status, err := hermes.TaskStatus(ctx, taskID)
				if err != nil {
					logger.Error("card status failed", "task", taskID, "error", err.Error())
					continue
				}
				if status == "running" || status == "review" {
					continue
				}
			}
			if err := services.Store.RecoverLostClaim(ctx, run.Key, run.ClaimedAt, time.Now().UTC()); err != nil {
				logger.Error("claim recovery failed", "run", run.RunID, "error", err.Error())
				continue
			}
			logger.Info("dead claim returned to the queue", "run", run.RunID, "claimed_at", run.ClaimedAt)
		case "awaiting_answer":
			// The runner blocks its own card before exiting; this is the
			// recovery pass for a card that escaped (e.g. the runner died
			// after posting the question but before blocking).
			if taskID == "" {
				continue
			}
			status, err := hermes.TaskStatus(ctx, taskID)
			if err != nil {
				logger.Error("card status failed", "task", taskID, "error", err.Error())
				continue
			}
			if status != "blocked" && status != "running" {
				if err := hermes.Block(ctx, taskID, "awaiting-answer:"+run.DeliveryID); err != nil {
					logger.Error("card block failed", "task", taskID, "error", err.Error())
					continue
				}
				logger.Info("card re-blocked for waiting answer", "run", run.RunID, "task", taskID)
			}
		case "terminal":
			if taskID == "" {
				continue
			}
			// A runner that reports its own terminal exits 0/1 and the
			// supervisor retires the card; this pass retires the card of a
			// run the ATTENDANT terminated (answer deadline expiry, cancel)
			// — without it the card stays blocked forever while the ledger
			// has already closed the run.
			status, err := hermes.TaskStatus(ctx, taskID)
			if err != nil {
				logger.Error("card status failed", "task", taskID, "error", err.Error())
				continue
			}
			if status != "done" && status != "archived" {
				if err := hermes.Complete(ctx, taskID); err != nil {
					logger.Error("card complete failed", "task", taskID, "error", err.Error())
					continue
				}
				logger.Info("card completed for terminal run", "run", run.RunID, "task", taskID)
			}
			delete(cards.Cards, run.DeliveryID)
			changed = true
		}
	}
	if changed {
		return cards.save()
	}
	return nil
}
