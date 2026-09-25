package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
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

	// The investigating designer's stages (docs/INVESTIGATING_DESIGNER.md
	// §9). They count in design rounds (`:d<N>` keys), the stages above in
	// implementation rounds (`:r<N>`), because a design round can restart
	// without an implementation round having happened.
	StageInvestigate   = "investigate"
	StageDesignReviewA = "design-review-a"
	StageDesignReviewB = "design-review-b"
	StageDesignDecide  = "design-decide"
	// StageApply is the light implementer that copies an approved design.
	// It counts in implementation rounds like the implement card it replaces.
	StageApply = "apply"
)

// ChainShape selects which cards a delivery runs.
type ChainShape string

const (
	// ShapeImplement is the original chain: implement, two reviews, validate, publish.
	ShapeImplement ChainShape = "implement"
	// ShapeInvestigation ends with a sealed investigation report: investigate,
	// one evidence review and the decision (or investigate alone when the
	// consumer turned the report review off).
	ShapeInvestigation ChainShape = "investigation"
	// ShapeDesign measures, designs, has the design reviewed, then applies
	// it: investigate, two design reviews, design-decide, apply, and the
	// original review / validate / publish tail.
	ShapeDesign ChainShape = "design"
)

// ChainPlan is the shape plus the consumer switches that vary it.
type ChainPlan struct {
	Shape ChainShape
	// ReviewInvestigation adds the evidence review to an investigation-only
	// delivery (consumer `design.review_investigation`, default on).
	ReviewInvestigation bool
	// Reviewers is how many judges the consumer configured
	// (`models.reviewers`). One judge runs one review card instead of two:
	// a second opinion on a change only pays for itself where being wrong
	// is expensive, and where it does not, the two have to agree before
	// anything ships. Two live deliveries died that way on 2026-09-17,
	// deadlocked over a judgement call a single model settles in one pass.
	// Zero means the caller did not say, and the chain keeps its original
	// two cards.
	Reviewers int
}

// reviewCards is how many review cards a plan runs. Only two review
// profiles exist, so a consumer with more judges than that still runs two
// cards and gives each card more than one judge.
func (p ChainPlan) reviewCards() int {
	if p.Reviewers == 1 {
		return 1
	}
	return 2
}

// IsDesignStage reports whether a stage counts in design rounds.
func IsDesignStage(stage string) bool {
	switch stage {
	case StageInvestigate, StageDesignReviewA, StageDesignReviewB, StageDesignDecide:
		return true
	}
	return false
}

// The delivery's own cards. They are not chain stages — they run outside
// the chain's namespace, after the publish card is done, and they have no
// rounds of their own: a change is merged, deployed and observed once,
// however many times each of those is attempted. They are named here
// because the failure record a card seals has to be found again by the
// tick that reads it, and two copies of these names would drift.
const (
	DeliverStageChecks    = "checks"
	DeliverStageIntegrate = "integrate"
	DeliverStagePromote   = "promote"
	// DeliverRound is the round a delivery card's records are kept under.
	DeliverRound = 1
)

// IsDeliverStage reports whether a stage name is one of the delivery cards.
func IsDeliverStage(stage string) bool {
	switch stage {
	case DeliverStageChecks, DeliverStageIntegrate, DeliverStagePromote:
		return true
	}
	return false
}

// knownStage reports whether a stage name is one the chain runs.
// AllStages is every stage a chain can run. It is one list because two
// copies of it drift: anything that must cover every stage takes it from
// here rather than restating the names, so a stage added without being
// covered fails rather than passing quietly.
func AllStages() []string {
	return []string{
		StageImplement, StageReviewA, StageReviewB, StageValidate, StagePublish,
		StageInvestigate, StageDesignReviewA, StageDesignReviewB, StageDesignDecide, StageApply,
	}
}

func knownStage(stage string) bool {
	return slices.Contains(AllStages(), stage)
}

// DispatchedStages is every card the engine dispatches for one delivery:
// the chain's rounds and the delivery continuation's three phases. The
// delivery cards are absent from AllStages because that list builds a
// round and they belong to none, so this is the list for anything that
// must name every card — a credential's stages above all, where a name
// reaching no card would hand its secret to nothing and say nothing
// about it.
func DispatchedStages() []string {
	return append(AllStages(), DeliverStageChecks, DeliverStageIntegrate, DeliverStagePromote)
}

// DispatchedStage reports whether a name is one of them.
func DispatchedStage(stage string) bool {
	return slices.Contains(DispatchedStages(), stage)
}

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

// cardWallMargin is how far inside its own wall the process running a card
// stops itself.
//
// The wall is enforced from outside: the attendant hands it to the kanban
// as --max-runtime and the supervisor sends SIGTERM when it expires. A
// signal is indistinguishable, from inside, from the pod being replaced —
// and the two mean opposite things, one being a card that could not finish
// in its time and the other a card that never got its time. Stopping a
// minute early lets the card find out which of the two happened to it,
// which is the whole difference between a failure that is counted and one
// that is replayed for free. A minute is long enough to cover the gap
// between the supervisor starting the process and the process reaching
// this, and short enough to lose nothing on walls measured in tens of
// minutes.
const cardWallMargin = time.Minute

// CardWall is the runtime bound the card for one stage is dispatched with:
// the same number the attendant hands the kanban. A name no card carries —
// or one whose card is dispatched without a bound — has no wall, and the
// caller is then bounded by nothing but the supervisor.
func CardWall(chain ChainConfig, stage string) time.Duration {
	// Both shapes, because a stage belongs to whichever one runs it and
	// this is asked by a card that knows only its own name. The plan is
	// left at its zero value on purpose: that is the widest chain either
	// shape builds, so both review cards are in the list.
	for _, shape := range []ChainShape{ShapeImplement, ShapeDesign, ShapeInvestigation} {
		for _, candidate := range ChainStagesFor(chain, ChainPlan{Shape: shape, ReviewInvestigation: true}) {
			if candidate.Name == stage && candidate.MaxRuntimeSeconds > 0 {
				return time.Duration(candidate.MaxRuntimeSeconds) * time.Second
			}
		}
	}
	switch stage {
	case DeliverStageChecks:
		return time.Duration(chain.Deliver.ChecksWallSeconds()) * time.Second
	case DeliverStageIntegrate:
		return time.Duration(chain.Deliver.IntegrateWallSeconds()) * time.Second
	case DeliverStagePromote:
		return time.Duration(chain.Deliver.PromoteWallSeconds()) * time.Second
	}
	return 0
}

// WithWall bounds ctx by a card's wall, less the margin, so the process
// reaches its own deadline before the supervisor's signal reaches it.
//
// A wall shorter than the margin keeps half of itself rather than a
// deadline that has already passed: every real wall is tens of minutes, and
// a configuration that sets a tiny one still wants the card to stop itself
// first. No wall at all leaves ctx alone.
func WithWall(ctx context.Context, wall time.Duration) (context.Context, context.CancelFunc) {
	if wall <= 0 {
		return ctx, func() {}
	}
	within := wall - cardWallMargin
	if within <= 0 {
		within = wall / 2
	}
	return context.WithTimeout(ctx, within)
}

// ChainStages is the original chain for one configuration, in order.
func ChainStages(chain ChainConfig) []ChainStage {
	return ChainStagesFor(chain, ChainPlan{Shape: ShapeImplement})
}

// ChainStagesFor is the chain for one plan, in order.
func ChainStagesFor(chain ChainConfig, plan ChainPlan) []ChainStage {
	tail := implementChainStages(chain, plan.reviewCards())
	switch plan.Shape {
	case ShapeInvestigation:
		stages := []ChainStage{investigateStage(chain)}
		if plan.ReviewInvestigation {
			stages = append(stages,
				ChainStage{Name: StageDesignReviewA, Profile: chain.Profiles.DesignReviewA, MaxRuntimeSeconds: 70 * 60},
				ChainStage{Name: StageDesignDecide, Profile: chain.Profiles.DesignDecide, MaxRuntimeSeconds: 5 * 60},
			)
		}
		return stages
	case ShapeDesign:
		stages := []ChainStage{
			investigateStage(chain),
			// The design reviews carry the implementation reviews' wall:
			// the same agents judge, only the subject differs.
			{Name: StageDesignReviewA, Profile: chain.Profiles.DesignReviewA, MaxRuntimeSeconds: 70 * 60},
		}
		if plan.reviewCards() > 1 {
			stages = append(stages, ChainStage{Name: StageDesignReviewB, Profile: chain.Profiles.DesignReviewB, MaxRuntimeSeconds: 70 * 60})
		}
		stages = append(stages,
			// design-decide is a kernel process like validate; five minutes
			// outlasts reading the reviews and sealing a decision.
			ChainStage{Name: StageDesignDecide, Profile: chain.Profiles.DesignDecide, MaxRuntimeSeconds: 5 * 60},
			// The applier copies a reviewed design: 40 turns at the measured
			// 20 seconds each is 800 seconds; the wall leaves room for the
			// seal that follows on the next card.
			ChainStage{Name: StageApply, Profile: chain.Profiles.Applier, MaxRuntimeSeconds: 20 * 60},
		)
		return append(stages, tail[1:]...)
	default:
		return tail
	}
}

// investigateStage is the kernel-driven investigating designer. The role's
// own budget is 1,800 seconds (docs/INVESTIGATING_DESIGNER.md §3.1); the
// card wall must outlast it plus the seal, or the kanban SIGTERM kills a
// working round from outside — the same lesson the review walls record.
func investigateStage(chain ChainConfig) ChainStage {
	return ChainStage{Name: StageInvestigate, Profile: chain.Profiles.Investigate, MaxRuntimeSeconds: 40 * 60}
}

func implementChainStages(chain ChainConfig, reviewCards int) []ChainStage {
	stages := []ChainStage{
		{Name: StageImplement, Profile: chain.Profiles.Implementer, MaxRuntimeSeconds: 90 * 60},
		// The card wall must outlast the reviewing agent's own budget
		// (agents.reviewer_agents timeout, 60 minutes) plus sealing
		// overhead, or the kanban SIGTERM kills a working review from
		// outside: both live runs of two tickets died at exactly this
		// wall while their reviewers were mid-judgment.
		{Name: StageReviewA, Profile: chain.Profiles.ReviewA, MaxRuntimeSeconds: 70 * 60},
	}
	if reviewCards > 1 {
		stages = append(stages, ChainStage{Name: StageReviewB, Profile: chain.Profiles.ReviewB, MaxRuntimeSeconds: 70 * 60})
	}
	return append(stages, validateAndPublishStages(chain)...)
}

// validateAndPublishStages close every implementing chain.
func validateAndPublishStages(chain ChainConfig) []ChainStage {
	return []ChainStage{
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

// ChainCardKey is the idempotency key of one stage card. Design stages
// count in design rounds (`:d<N>`), the others in implementation rounds
// (`:r<N>`), so a second design round never collides with the finished
// cards of the first.
func ChainCardKey(deliveryID, stage string, round int) string {
	if IsDesignStage(stage) {
		return fmt.Sprintf("%s:%s:d%d", deliveryID, stage, round)
	}
	return fmt.Sprintf("%s:%s:r%d", deliveryID, stage, round)
}

// ParseChainCardKey splits a chain card key back into its parts. The second
// return is false for keys of other shapes. The round returned is the
// design round for design stages and the implementation round otherwise.
func ParseChainCardKey(key string) (deliveryID, stage string, round int, ok bool) {
	last := strings.LastIndex(key, ":")
	if last <= 0 || last+2 > len(key) {
		return "", "", 0, false
	}
	letter := key[last+1]
	if letter != 'r' && letter != 'd' {
		return "", "", 0, false
	}
	if _, err := fmt.Sscanf(key[last+2:], "%d", &round); err != nil || round < 1 {
		return "", "", 0, false
	}
	rest := key[:last]
	middle := strings.LastIndex(rest, ":")
	if middle <= 0 {
		return "", "", 0, false
	}
	deliveryID, stage = rest[:middle], rest[middle+1:]
	if !knownStage(stage) || IsDesignStage(stage) != (letter == 'd') {
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
	return EnsureChainFor(ctx, hermes, chain, ChainPlan{Shape: ShapeImplement}, existing, deliveryID, runID, summary, ChainRounds{Implement: round})
}

// ChainRounds are the two counters a delivery's cards live in.
type ChainRounds struct {
	// Design is the current design round (investigate and the design
	// reviews); 0 for a shape without design stages.
	Design int
	// Implement is the current implementation round (implement or apply and
	// the tail); 0 for a shape without implementation stages.
	Implement int
}

func (r ChainRounds) roundFor(stage string) int {
	if IsDesignStage(stage) {
		return r.Design
	}
	return r.Implement
}

// EnsureChainFor creates the missing cards of one plan, keyed by the round
// each stage counts in, chained parent to child, after the last living
// card. Rounds must be positive for every stage the plan contains.
func EnsureChainFor(
	ctx context.Context,
	hermes *Hermes,
	chain ChainConfig,
	plan ChainPlan,
	existing map[string]BoardTask,
	deliveryID, runID, summary string,
	rounds ChainRounds,
) (string, error) {
	stages := ChainStagesFor(chain, plan)
	for _, stage := range stages {
		if rounds.roundFor(stage.Name) < 1 {
			return "", fmt.Errorf("chain round for stage %s is invalid", stage.Name)
		}
	}
	workspace := "dir:" + RunDirectory(chain, deliveryID)
	// The last stage with a living card gates everything before it: the
	// kanban treats an archived parent as satisfied, so recreating an
	// earlier stage would hand it straight to dispatch and run it in
	// parallel with the chain's living remainder on the shared workspace
	// (buddy-review finding, measured against the fork's ready
	// recomputation). Gaps before a living stage are therefore left alone;
	// creation resumes only after the last living card.
	lastLive := -1
	for index, stage := range stages {
		if task, ok := existing[ChainCardKey(deliveryID, stage.Name, rounds.roundFor(stage.Name))]; ok && task.Status != "archived" {
			lastLive = index
		}
	}
	parent := ""
	terminal := ""
	for index, stage := range stages {
		round := rounds.roundFor(stage.Name)
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
		letter := "r"
		if IsDesignStage(stage.Name) {
			letter = "d"
		}
		title := fmt.Sprintf("%s %s %s%d", runID, stage.Name, letter, round)
		if summary != "" {
			title = fmt.Sprintf("%s %s %s%d: %s", runID, stage.Name, letter, round, summary)
		}
		body := fmt.Sprintf(
			"Automated ticket run, stage %s of round %d.\nDelivery: %s\nTicket: %s\n\nDispatched by the LassDas attendant; the assignee profile runs this stage in the shared run directory.",
			stage.Name, round, deliveryID, runID,
		)
		if stage.Name == StageImplement || stage.Name == StageApply {
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
