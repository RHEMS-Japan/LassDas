package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// The cards orchestration runs one delivery as a chain of stage cards,
// implement → review-a → review-b → validate → publish, linked parent to
// child so a stage dispatches only after the one before it finished. The
// chain replaces the runner's in-process sequencing with the kanban's own
// (docs/M2_MIGRATION.md); the policy kernel's verbs stay the authority on
// what each stage may produce.
//
// A round is a fresh set of cards: a revise decision retires the round's
// remnant and the next round starts at implement again, on the same
// delivery directory. The idempotency key of every card carries the
// delivery, the stage and the round, so recreating an existing round is a
// no-op card by card — which is also what heals a chain whose creation
// crashed halfway.

// Chain stage names, in execution order. The names are part of the card
// identity (idempotency keys) — renaming one orphans in-flight rounds.
const (
	StageImplement = "implement"
	StageReviewA   = "review-a"
	StageReviewB   = "review-b"
	StageValidate  = "validate"
	StagePublish   = "publish"
)

// ChainStage is one step of the chain: which profile runs it and how long
// it may take. The runtimes are the operating premise from the migration
// design (implement 90 minutes, reviews and validation 30, publish 15);
// direct-command workers emit no heartbeats, so max-runtime is the only
// stall bound each card has.
type ChainStage struct {
	Name              string
	Profile           string
	MaxRuntimeSeconds int
}

// ChainStages is the chain for one configuration, in order.
func ChainStages(chain ChainConfig) []ChainStage {
	return []ChainStage{
		{Name: StageImplement, Profile: chain.Profiles.Implementer, MaxRuntimeSeconds: 90 * 60},
		// The card wall must outlast the reviewing agent's own budget
		// (agents.reviewer_agents timeout, 60 minutes) plus sealing
		// overhead, or the kanban SIGTERM kills a working review from
		// outside: both live runs of RFDEV-618/619 died at exactly this
		// wall while their reviewers were mid-judgment.
		{Name: StageReviewA, Profile: chain.Profiles.ReviewA, MaxRuntimeSeconds: 70 * 60},
		{Name: StageReviewB, Profile: chain.Profiles.ReviewB, MaxRuntimeSeconds: 70 * 60},
		// The validate card runs the decision plus install and up to four
		// verify commands, each with its own ten-minute ceiling
		// (ValidationCommandTimeout) — a legal consumer configuration can
		// spend fifty minutes before anything hangs, so thirty (the design
		// sketch's number) would time out honest work. The publish card
		// below carries the same bound for the same reason: its
		// base-advance retry re-runs this exact validation on the freshly
		// advanced base before publishing again.
		{Name: StageValidate, Profile: chain.Profiles.Validate, MaxRuntimeSeconds: 60 * 60},
		{Name: StagePublish, Profile: chain.Profiles.Publish, MaxRuntimeSeconds: 60 * 60},
	}
}

// ChainCardKey is the idempotency key of one stage card.
func ChainCardKey(deliveryID, stage string, round int) string {
	return fmt.Sprintf("%s:%s:r%d", deliveryID, stage, round)
}

// ParseChainCardKey splits a chain card key back into its parts. The second
// return is false for keys of other shapes (the runner mode's plain
// delivery-id cards above all).
func ParseChainCardKey(key string) (deliveryID, stage string, round int, ok bool) {
	last := strings.LastIndex(key, ":r")
	if last <= 0 {
		return "", "", 0, false
	}
	if _, err := fmt.Sscanf(key[last:], ":r%d", &round); err != nil || round < 1 {
		return "", "", 0, false
	}
	rest := key[:last]
	middle := strings.LastIndex(rest, ":")
	if middle <= 0 {
		return "", "", 0, false
	}
	deliveryID, stage = rest[:middle], rest[middle+1:]
	switch stage {
	case StageImplement, StageReviewA, StageReviewB, StageValidate, StagePublish:
	default:
		return "", "", 0, false
	}
	// Only the canonical spelling round-trips; Sscanf alone would accept
	// signs, leading zeros and trailing bytes (measured by a review probe).
	if ChainCardKey(deliveryID, stage, round) != key {
		return "", "", 0, false
	}
	return deliveryID, stage, round, true
}

// RunDirectory is the delivery's shared working directory under the
// configured root: every card of every round of one delivery mounts it as
// its explicit workspace.
func RunDirectory(chain ChainConfig, deliveryID string) string {
	return filepath.Join(chain.RunsRoot, deliveryID)
}

// EnsureChain creates the round's missing cards, in order, each child of
// the stage before it. existing maps idempotency key to the known card for
// it (built from the kanban's own listing — never from a cache), which
// makes the call idempotent and lets it heal a chain whose creation died
// halfway: stages that exist are kept, missing ones are created with their
// parent taken from the stage before, found or fresh. It returns the id of
// the publish card — the chain's terminal — for the caller's bookkeeping.
func EnsureChain(
	ctx context.Context,
	hermes *Hermes,
	chain ChainConfig,
	existing map[string]BoardTask,
	deliveryID, runID, summary string,
	round int,
) (string, error) {
	if round < 1 {
		return "", fmt.Errorf("chain round %d is invalid", round)
	}
	workspace := "dir:" + RunDirectory(chain, deliveryID)
	stages := ChainStages(chain)
	// The last stage with a living card gates everything before it: the
	// kanban treats an archived parent as satisfied, so recreating an
	// earlier stage would hand it straight to dispatch and run it in
	// parallel with the chain's living remainder on the shared workspace
	// (buddy-review finding, measured against the fork's ready
	// recomputation). Gaps before a living stage are therefore left alone;
	// creation resumes only after the last living card.
	lastLive := -1
	for index, stage := range stages {
		if task, ok := existing[ChainCardKey(deliveryID, stage.Name, round)]; ok && task.Status != "archived" {
			lastLive = index
		}
	}
	parent := ""
	terminal := ""
	for index, stage := range stages {
		key := ChainCardKey(deliveryID, stage.Name, round)
		if task, ok := existing[key]; ok && task.Status != "archived" {
			parent = task.ID
			terminal = task.ID
			continue
		}
		if index <= lastLive {
			// A hole before a living stage is already satisfied as far as
			// the kanban's gating is concerned; nothing safe to recreate.
			continue
		}
		title := fmt.Sprintf("%s %s r%d", runID, stage.Name, round)
		if summary != "" {
			title = fmt.Sprintf("%s %s r%d: %s", runID, stage.Name, round, summary)
		}
		body := fmt.Sprintf(
			"Automated ticket run, stage %s of round %d.\nDelivery: %s\nTicket: %s\n\nDispatched by the LassDas attendant; the assignee profile runs this stage in the shared run directory.",
			stage.Name, round, deliveryID, runID,
		)
		if stage.Name == StageImplement {
			// The implement card runs a native agent whose whole prompt is
			// the kernel-rendered instruction file the attendant placed in
			// the shared directory — the card body must not become a second
			// instruction channel.
			body = fmt.Sprintf(
				"Delivery: %s\nTicket: %s (round %d)\n\n作業ディレクトリ直下の INSTRUCTION.md を読み、その指示に従って実装してください。指示は INSTRUCTION.md が全てで、このカード本文に追加の指示はありません。",
				deliveryID, runID, round,
			)
		}
		created, err := hermes.CreateTask(ctx, CardSpec{
			Title: title, Body: body, Assignee: stage.Profile,
			IdempotencyKey: key, Parent: parent, Workspace: workspace,
			MaxRuntimeSeconds: stage.MaxRuntimeSeconds, CreatedBy: "lassdas-attendant",
		})
		if err != nil {
			return "", fmt.Errorf("chain stage %s: %w", stage.Name, err)
		}
		parent = created
		terminal = created
	}
	return terminal, nil
}
