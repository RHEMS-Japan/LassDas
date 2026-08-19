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
		return nil, fmt.Errorf("cards.json is corrupt: %w", err)
	}
	return ledger, nil
}

func (l *CardLedger) save() error {
	encoded, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.path, encoded, 0o600)
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

// Block parks a card (used by the runner for its own card while a question
// waits; the reason string is the human-facing half, §3.1).
func (h *Hermes) Block(ctx context.Context, taskID, reason string) error {
	_, err := h.run(ctx, "block", taskID, "--reason", reason)
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
			// A queued run with an existing card is a resumed run: the
			// adopted answer returned it to the queue while its card sits
			// blocked. Unblock is idempotent enough — a card that is not
			// blocked makes this a no-op error we only log.
			status, err := hermes.TaskStatus(ctx, taskID)
			if err != nil {
				logger.Error("card status failed", "task", taskID, "error", err.Error())
				continue
			}
			if status == "blocked" {
				if err := hermes.Unblock(ctx, taskID); err != nil {
					logger.Error("card unblock failed", "task", taskID, "error", err.Error())
					continue
				}
				logger.Info("card unblocked", "run", run.RunID, "task", taskID)
			}
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
			if taskID != "" {
				delete(cards.Cards, run.DeliveryID)
				changed = true
			}
		}
	}
	if changed {
		return cards.save()
	}
	return nil
}
