package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Hermes drives the hermes CLI — the canonical channel for every card
// transition and read (docs/RUNTIME_POD.md: no direct kanban.db access,
// ever).
type Hermes struct {
	bin   string
	board string
}

func NewHermes(config Config) *Hermes {
	bin := config.HermesBin
	if bin == "" {
		bin = "hermes"
	}
	return &Hermes{bin: bin, board: config.HermesBoard}
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
	// BlockKind is the block's kind for blocked cards (needs_input,
	// auto, …); empty otherwise.
	BlockKind string `json:"block_kind"`
}

// ListBoardTasks reads every card on the board, archived ones included,
// whatever profile holds it. The chain spreads one delivery across five
// assignees, so an assignee-scoped listing cannot see a whole chain; the
// board is dedicated to this automation, which is what makes the unscoped
// read sound.
func (h *Hermes) ListBoardTasks(ctx context.Context) ([]BoardTask, error) {
	output, err := h.run(ctx, "list", "--archived", "--json")
	if err != nil {
		return nil, err
	}
	var tasks []BoardTask
	if err := json.Unmarshal(output, &tasks); err != nil {
		return nil, fmt.Errorf("hermes kanban list returned no task array: %s", strings.TrimSpace(string(output)))
	}
	return tasks, nil
}

// CardSpec is one card to create through the canonical CLI. Zero-valued
// optional fields are omitted from the command line.
type CardSpec struct {
	Title          string
	Body           string
	Assignee       string
	IdempotencyKey string
	// Parent, when set, makes this card a child that stays out of dispatch
	// until the parent is done — the arrow of the stage chain.
	Parent string
	// Workspace is the card's --workspace value (the chain passes an
	// explicit dir:<path> so every stage works on the same tree).
	Workspace         string
	MaxRuntimeSeconds int
	CreatedBy         string
}

// CreateTask creates one card from a spec (idempotently, by its key).
func (h *Hermes) CreateTask(ctx context.Context, spec CardSpec) (string, error) {
	arguments := []string{"create", spec.Title, "--body", spec.Body, "--assignee", spec.Assignee,
		"--idempotency-key", spec.IdempotencyKey, "--created-by", spec.CreatedBy, "--json"}
	if spec.Parent != "" {
		arguments = append(arguments, "--parent", spec.Parent)
	}
	if spec.Workspace != "" {
		arguments = append(arguments, "--workspace", spec.Workspace)
	}
	if spec.MaxRuntimeSeconds > 0 {
		arguments = append(arguments, "--max-runtime", fmt.Sprintf("%d", spec.MaxRuntimeSeconds))
	}
	output, err := h.run(ctx, arguments...)
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

// Archive retires a card unconditionally and — unlike done — releases its
// idempotency key, so a fresh card can be created for the same delivery.
func (h *Hermes) Archive(ctx context.Context, taskID string) error {
	_, err := h.run(ctx, "archive", taskID)
	return err
}
