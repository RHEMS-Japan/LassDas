package worker

import (
	"automation.internal/ticket-ingress/internal/probe"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"automation.internal/ticket-ingress/internal/githubapi"
)

const ConfigSchemaVersion = 4

var (
	identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	versionPattern    = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,2}$`)
	sha256Pattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	commitPattern     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	deliveryPattern   = regexp.MustCompile(`^delivery_[a-f0-9]{32}$`)
	// The run id's shape is the reception's to define (hook.ValidRunID); a
	// second copy here drifted once — the reception admitted a short ticket
	// key that this package then refused as an invalid ticket identity.
	issueKeyPattern         = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}-[1-9][0-9]*$`)
	relativePathPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,511}$`)
	verificationPathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9._~-]+/)*[A-Za-z0-9._~-]*$`)
	modelRequestIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	apiKeyEnvPattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	envNamePattern          = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	vendorHostPattern       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// Config carries one delivery destination per consumer repository. Which
// repository a ticket belongs to is read out of the ticket, so everything that
// differs between destinations — branches, verification, writable scope —
// lives inside the matching ConsumerConfig rather than in framework code.
type Config struct {
	SchemaVersion int              `json:"schema_version"`
	Consumers     []ConsumerConfig `json:"consumers"`
	Models        ModelConfig      `json:"models"`
	Agents        AgentSet         `json:"agents"`
	MaxStages     int              `json:"max_stages"`
	// Probes is the investigating designer's catalogue of read-only
	// measurements (docs/INVESTIGATING_DESIGNER.md §3.2): the shapes the
	// kernel may execute for the role, declared by the consumer. Optional;
	// without it the role can only read the repository.
	Probes []probe.Spec `json:"probes,omitempty"`
	// AnswerKnowledge names where adopted answers are preserved, if anywhere.
	AnswerKnowledge *AnswerKnowledgeConfig `json:"answer_knowledge,omitempty"`
	// DesignMaxRounds bounds the design review rounds: a design (or an
	// investigation report) the reviewers still object to at this round
	// ends the delivery as nonconverged instead of going on. Zero means
	// DefaultDesignMaxRounds; omitempty keeps existing configurations'
	// digests unchanged.
	DesignMaxRounds int `json:"design_max_rounds,omitempty"`
	// MaxRounds bounds the implementation rounds an operator is willing to
	// pay for. Zero — the default, and what every configuration written
	// before this says — means unbounded: a round that does not converge is
	// followed by another one, and a delivery that repeats itself is ended
	// by the arbiter rather than by a count. An operator who sets a number
	// gets a delivery that stops at it.
	MaxRounds int `json:"max_rounds,omitempty"`
	// StagnationRepeatRounds is how many consecutive rounds must be
	// identical before the engine rules on the deadlock. Zero means
	// DefaultStagnationRepeatRounds. omitempty keeps existing
	// configurations' digests unchanged.
	StagnationRepeatRounds int `json:"stagnation_repeat_rounds,omitempty"`

	// The reception's asking policy. All four are zero-valued when the
	// destination says nothing, and every accessor answers the default for
	// zero, so a configuration written before these existed keeps its exact
	// canonical form — and with it the digest every sealed record of a run
	// in flight is bound to.
	//
	// Questions is how much the reception may ask at all
	// (QuestionsNone | QuestionsMinimal | QuestionsNormal);
	// QuestionMaxItems how many questions one set may hold;
	// QuestionMaxRounds how many times a run may ask the requester
	// anything, which is once; QuestionDeadlineWeekdays how long they have
	// to answer.
	Questions                string `json:"questions,omitempty"`
	QuestionMaxItems         int    `json:"question_max_items,omitempty"`
	QuestionMaxRounds        int    `json:"question_max_rounds,omitempty"`
	QuestionDeadlineWeekdays int    `json:"question_deadline_weekdays,omitempty"`
	// AssumptionMaxItems bounds the points the reception may settle by
	// itself and record. Asking less means deciding more, so the record of
	// what was decided is where the reception's work now shows.
	AssumptionMaxItems int `json:"assumption_max_items,omitempty"`

	// finishedRunSHA256 pins the digest this configuration's sealed records
	// are held to. It is unexported so that SHA256, which marshals the
	// exported fields, still reports the configuration's own digest, and so
	// that nothing sealed while the pin is held can inherit it. Empty — the
	// zero value — is the live configuration, which is what every path that
	// loads a configuration from disk gets. See ForFinishedRun.
	finishedRunSHA256 string
}

// DefaultDesignMaxRounds is the design review round limit a configuration
// gets when it sets none.
const DefaultDesignMaxRounds = 3

// DefaultStagnationRepeatRounds is how many consecutive identical rounds
// make a deadlock when a configuration sets no number: one. A round that
// objects to exactly what the round before it objected to, or that changes
// not one byte of what that round changed, has already shown that asking
// again produces the same thing.
const DefaultStagnationRepeatRounds = 1

// StageCeiling is the highest round number any sealed record may carry.
//
// It is not a budget. Rounds end when the reviewers agree or when the
// arbiter rules, and neither of those is a count; this is the bound every
// record's round field is read and written under, so that a corrupt or
// forged number is refused by the same rule everywhere. Fifty is far above
// any delivery that has ever been seen and far below a number that could
// make a run directory unreadable.
const StageCeiling = 50

// DesignRounds is the design review round budget a configuration declares.
//
// It no longer stops a design: a design whose judges keep objecting is
// ruled on, not abandoned at a count (§6 of the plan). What the declared
// number still does is take part in the artifact ceiling below, so a
// configuration that declares more rounds than the ceiling allows cannot
// seal a record the ceiling would refuse.
func (c Config) DesignRounds() int {
	if c.DesignMaxRounds == 0 {
		return DefaultDesignMaxRounds
	}
	return c.DesignMaxRounds
}

// StageCeiling is the highest round number this configuration's records may
// carry: the ceiling, or a declared budget above it.
func (c Config) StageCeiling() int {
	ceiling := StageCeiling
	if rounds := c.DesignRounds(); rounds > ceiling {
		ceiling = rounds
	}
	if c.MaxStages > ceiling {
		ceiling = c.MaxStages
	}
	return ceiling
}

// RoundLimit is the number of implementation rounds this configuration
// allows, or zero for unbounded.
func (c Config) RoundLimit() int {
	if c.MaxRounds < 1 {
		return 0
	}
	return c.MaxRounds
}

// StagnationRounds is how many consecutive identical rounds make a deadlock.
func (c Config) StagnationRounds() int {
	if c.StagnationRepeatRounds < 1 {
		return DefaultStagnationRepeatRounds
	}
	return c.StagnationRepeatRounds
}

// maxRunStage bounds the stage an agent run record may carry. An
// implementing or reviewing run belongs to an implementation stage and a
// design reviewer's run to a design round; both are held to the one
// ceiling every other round-numbered record is held to.
func (c Config) maxRunStage() int {
	return c.StageCeiling()
}

// AgentSet names the coding agents the framework runs: one that implements
// the change by working in the repository, and the ones that review the
// result. The reviewer must not be able to be the author of what it reviews:
// different programs separate by themselves, and one shared program (the M2
// shape — both roles are hermes) is admitted only under the separation
// separatedAgents states.
type AgentSet struct {
	Implementer AgentConfig `json:"implementer"`
	Reviewer    AgentConfig `json:"reviewer"`
	// Applier, when present, is the launch of the light implementer that
	// copies an approved design (docs/INVESTIGATING_DESIGNER.md §7). A
	// design-backed delivery without it fails at its apply card, closed.
	// A pointer so an absent applier leaves the configuration's canonical
	// form — and every digest bound to it — exactly as it was.
	Applier *AgentConfig `json:"applier,omitempty"`
	// ReviewerAgents, when present, gives a reviewer endpoint its own launch
	// definition — its own profile, its own credential source — instead of
	// the shared Reviewer one. A reviewer without an entry keeps the shared
	// definition, which is what keeps existing configurations meaning what
	// they meant.
	ReviewerAgents []ReviewerAgent `json:"reviewer_agents,omitempty"`
	// DesignReviewerAgents, when present, give the design judges their own
	// launch definitions (a profile of their own, their own credential
	// source), one per reviewer id, all or none. Unset, a design review is
	// launched as the candidate review of the same id.
	DesignReviewerAgents []ReviewerAgent `json:"design_reviewer_agents,omitempty"`
}

// ReviewerAgent binds one reviewer endpoint to the launch that runs its
// review. The binding is part of the sealed configuration, so a review run
// record (agent id + config digest) pins down which profile and credential
// source judged the change.
type ReviewerAgent struct {
	ReviewerID string      `json:"reviewer_id"`
	Agent      AgentConfig `json:"agent"`
	// Candidates are the launches for the seat's candidate endpoints, in
	// the same order. A seat moves to another vendor by being launched
	// differently — another profile, another credential source — so an
	// endpoint candidate without a launch beside it would name a second
	// provider and go on talking to the first. Validation holds the two
	// lists to the same length for that reason. Omitted when empty, which
	// leaves an existing configuration's encoding untouched.
	Candidates []AgentConfig `json:"candidates,omitempty"`
}

// AgentFor is the launch for one occupant of this seat: place 0 is the
// configured launch, place 1 the first candidate's.
func (r ReviewerAgent) AgentFor(place int) (AgentConfig, bool) {
	if place == 0 {
		return r.Agent, true
	}
	if place < 1 || place > len(r.Candidates) {
		return AgentConfig{}, false
	}
	return r.Candidates[place-1], true
}

// applierConfigured reports whether the consumer gave the applier a launch.
func (a AgentSet) applierConfigured() bool { return a.Applier != nil }

func (a AgentSet) validate() error {
	if err := a.Implementer.validate(); err != nil {
		return fmt.Errorf("implementer agent: %w", err)
	}
	if err := a.Reviewer.validate(); err != nil {
		return fmt.Errorf("reviewer agent: %w", err)
	}
	ids := map[string]struct{}{a.Implementer.ID: {}}
	if _, exists := ids[a.Reviewer.ID]; exists {
		return errors.New("agent ids must differ")
	}
	ids[a.Reviewer.ID] = struct{}{}
	if a.applierConfigured() {
		if err := a.Applier.validate(); err != nil {
			return fmt.Errorf("applier agent: %w", err)
		}
		if _, exists := ids[a.Applier.ID]; exists {
			return errors.New("agent ids must differ")
		}
		ids[a.Applier.ID] = struct{}{}
	}
	if len(a.ReviewerAgents) > 4 {
		return errors.New("reviewer agents are invalid")
	}
	reviewers := make(map[string]struct{}, len(a.ReviewerAgents))
	for _, entry := range a.ReviewerAgents {
		if !identifierPattern.MatchString(entry.ReviewerID) {
			return errors.New("reviewer agent reviewer id is invalid")
		}
		if _, exists := reviewers[entry.ReviewerID]; exists {
			return errors.New("reviewer agent reviewer ids contain duplicates")
		}
		reviewers[entry.ReviewerID] = struct{}{}
		if err := entry.seatLaunches(ids); err != nil {
			return fmt.Errorf("reviewer agent %s: %w", entry.ReviewerID, err)
		}
	}
	// Every pair of launch definitions that can actually run must be
	// separated, not only the implementer against each judge: two judges
	// sharing one identity would be one veto seat counted twice. With
	// bindings present the shared reviewer definition is unreachable
	// (bindings are all or none, checked at the config level) and must not
	// force a phantom identity into the separation.
	if len(a.DesignReviewerAgents) > 4 {
		return errors.New("design reviewer agents are invalid")
	}
	judges := make(map[string]struct{}, len(a.DesignReviewerAgents))
	for _, entry := range a.DesignReviewerAgents {
		if !identifierPattern.MatchString(entry.ReviewerID) {
			return errors.New("design reviewer agent reviewer id is invalid")
		}
		if _, exists := judges[entry.ReviewerID]; exists {
			return errors.New("design reviewer agent reviewer ids contain duplicates")
		}
		judges[entry.ReviewerID] = struct{}{}
		if err := entry.seatLaunches(ids); err != nil {
			return fmt.Errorf("design reviewer agent %s: %w", entry.ReviewerID, err)
		}
	}
	launchable := []AgentConfig{a.Implementer}
	if a.applierConfigured() {
		launchable = append(launchable, *a.Applier)
	}
	if len(a.ReviewerAgents) == 0 {
		launchable = append(launchable, a.Reviewer)
	}
	// The candidate launches are in here too. They are alternatives to each
	// other and never run at once, but each of them runs against the other
	// seat and against the implementer — so a candidate that was another
	// launch under a different name would collapse the separation the
	// moment the seat moved onto it.
	for _, entry := range a.ReviewerAgents {
		launchable = append(launchable, entry.Agent)
		launchable = append(launchable, entry.Candidates...)
	}
	for _, entry := range a.DesignReviewerAgents {
		launchable = append(launchable, entry.Agent)
		launchable = append(launchable, entry.Candidates...)
	}
	for i := 0; i < len(launchable); i++ {
		for j := i + 1; j < len(launchable); j++ {
			if err := separatedAgents(launchable[i], launchable[j]); err != nil {
				return err
			}
		}
	}
	return nil
}

// separatedAgents verifies one launch cannot be another launch under a
// different name. Different programs separate by themselves — the original
// implementer-versus-reviewer rule this generalizes. The same program is
// admitted only when the two launches declare distinct profiles carried in
// their arguments and draw their credentials from distinct sources, so each
// is a different identity at the gateway even though the binary is shared.
func separatedAgents(left, right AgentConfig) error {
	if left.Command != right.Command {
		return nil
	}
	if left.Profile == "" || right.Profile == "" || left.Profile == right.Profile {
		return errors.New("same-program agents must declare distinct profiles")
	}
	// A profile carried in the other launch's arguments would let a
	// last-wins flag override the declared one — measured by an adversarial
	// probe appending the implementer's profile after the reviewer's own.
	if slices.Contains(left.Args, right.Profile) || slices.Contains(right.Args, left.Profile) {
		return errors.New("same-program agents must not carry each other's profile in their arguments")
	}
	if len(left.SecretEnv) == 0 || len(right.SecretEnv) == 0 {
		return errors.New("same-program agents must each hold a credential source")
	}
	sources := make(map[string]struct{}, len(left.SecretEnv))
	for _, source := range left.SecretEnv {
		sources[source] = struct{}{}
	}
	for _, source := range right.SecretEnv {
		if _, shared := sources[source]; shared {
			return errors.New("same-program agents must draw credentials from distinct sources")
		}
	}
	// A plain value under either side's secret variable name would hand a
	// run a literal credential, and with duplicate names in the child
	// environment the winner is decided by sort order — refused outright.
	secretNames := make(map[string]struct{}, len(left.SecretEnv)+len(right.SecretEnv))
	for name := range left.SecretEnv {
		secretNames[name] = struct{}{}
	}
	for name := range right.SecretEnv {
		secretNames[name] = struct{}{}
	}
	for name := range left.Env {
		if _, secret := secretNames[name]; secret {
			return errors.New("same-program agents must not shadow a secret variable")
		}
	}
	for name := range right.Env {
		if _, secret := secretNames[name]; secret {
			return errors.New("same-program agents must not shadow a secret variable")
		}
	}
	return nil
}

// byID selects the agent a run claims to be, so a run record cannot name an
// agent this configuration does not run.
func (a AgentSet) byID(id string) (AgentConfig, error) {
	switch id {
	case a.Implementer.ID:
		return a.Implementer, nil
	case a.Reviewer.ID:
		return a.Reviewer, nil
	}
	if a.Applier != nil && a.Applier.ID == id {
		return *a.Applier, nil
	}
	// Every occupant's launch, not only the configured one: a run sealed by
	// a seat that had moved names the launch that ran it, and a lookup that
	// stopped at the configured launches would refuse the delivery's own
	// record.
	for _, entry := range append(append([]ReviewerAgent(nil), a.ReviewerAgents...), a.DesignReviewerAgents...) {
		for _, launch := range append([]AgentConfig{entry.Agent}, entry.Candidates...) {
			if launch.ID == id {
				return launch, nil
			}
		}
	}
	return AgentConfig{}, errors.New("agent run names an agent that is not configured")
}

// DesignReviewerAgentFor picks the launch definition for one design judge:
// its own when the configuration gives it one, else the candidate
// reviewer's of the same id.
func (a AgentSet) DesignReviewerAgentFor(reviewerID string) AgentConfig {
	agent, _ := a.DesignReviewerAgentSeat(reviewerID, 0)
	return agent
}

// DesignReviewerAgentSeat is the same lookup for one occupant of the seat.
// A judge bound to its own launches uses that seat's list; one that falls
// back to the candidate reviewer's launch falls back to that seat's list
// too, so a design judge moves exactly as far as the launches allow.
func (a AgentSet) DesignReviewerAgentSeat(reviewerID string, place int) (AgentConfig, bool) {
	for _, entry := range a.DesignReviewerAgents {
		if entry.ReviewerID == reviewerID {
			return entry.AgentFor(place)
		}
	}
	return a.ReviewerAgentSeat(reviewerID, place)
}

// ReviewerAgentFor picks the launch definition for one reviewer endpoint:
// its own entry when the configuration carries one, the shared reviewer
// agent otherwise.
func (a AgentSet) ReviewerAgentFor(reviewerID string) AgentConfig {
	agent, _ := a.ReviewerAgentSeat(reviewerID, 0)
	return agent
}

// ReviewerAgentSeat is the launch for one occupant of a reviewer seat.
//
// Only place 0 exists for a seat on the shared reviewer definition: that
// launch is one program with one credential source, and handing it a
// second endpoint's work would run the new vendor's review through the old
// vendor's key. Validation refuses that configuration outright; this
// refuses it again at the moment of use, because a lookup that quietly
// returned the wrong launch would seal a review naming a model that never
// saw the change.
func (a AgentSet) ReviewerAgentSeat(reviewerID string, place int) (AgentConfig, bool) {
	for _, entry := range a.ReviewerAgents {
		if entry.ReviewerID == reviewerID {
			return entry.AgentFor(place)
		}
	}
	if place != 0 {
		return AgentConfig{}, false
	}
	return a.Reviewer, true
}

// ConsumerFor selects the destination a ticket names. The repository string is
// the exact "owner/name" form; nothing is guessed from partial matches.
func (c Config) ConsumerFor(repository string) (ConsumerConfig, error) {
	for _, consumer := range c.Consumers {
		if consumer.Repository == repository {
			return consumer, nil
		}
	}
	return ConsumerConfig{}, errors.New("repository is not a configured consumer")
}

// ConsumerRepositories lists the configured destinations in file order, for
// building the choices of a repository question.
func (c Config) ConsumerRepositories() []string {
	repositories := make([]string, 0, len(c.Consumers))
	for _, consumer := range c.Consumers {
		repositories = append(repositories, consumer.Repository)
	}
	return repositories
}

// SoleConsumer returns the only destination when exactly one is configured.
// Paths that predate the repository question (the fixed-header parser and its
// fixtures) stay valid for single-destination files and refuse to guess
// otherwise.
func (c Config) SoleConsumer() (ConsumerConfig, bool) {
	if len(c.Consumers) == 1 {
		return c.Consumers[0], true
	}
	return ConsumerConfig{}, false
}

// Delivery names how far a converged change travels without a person. It is a
// setting, not a property of the framework: what the automation owns is the
// rule for whether a change may be handed on at all, never where the consumer
// wants it handed to.
type Delivery string

const (
	// DeliverPullRequest stops once the change is proposed on the delivery
	// branch. Nothing is merged and nothing is deployed.
	DeliverPullRequest Delivery = "pull_request"
	// DeliverIntegration merges into the integration branch and confirms that
	// deployment, leaving the production decision to a person.
	DeliverIntegration Delivery = "integration"
	// DeliverProduction carries the change all the way and confirms it in
	// production. This is the maximum, not the default.
	DeliverProduction Delivery = "production"
)

func (d Delivery) valid() bool {
	switch d {
	case DeliverPullRequest, DeliverIntegration, DeliverProduction:
		return true
	default:
		return false
	}
}

// ReachesIntegration reports whether this setting merges anything.
func (d Delivery) ReachesIntegration() bool {
	return d == DeliverIntegration || d == DeliverProduction
}

// ReachesProduction reports whether this setting changes production.
func (d Delivery) ReachesProduction() bool { return d == DeliverProduction }

type ConsumerConfig struct {
	// Kind is web when omitted; cli proposes pull requests without web delivery.
	Kind         string `json:"kind,omitempty"`
	Repository   string `json:"repository"`
	RepositoryID int64  `json:"repository_id"`
	// Description says in one line what this destination is, in the
	// consumer's own words. Intake shows it when working out which
	// repository a ticket means, and it becomes the effect text of the
	// repository question when the ticket does not say.
	Description string `json:"description,omitempty"`
	// DeliveryBranch is the branch a change is proposed against. It defaults
	// to IntegrationBranch when unset so an existing configuration keeps its
	// meaning.
	DeliveryBranch     string   `json:"delivery_branch,omitempty"`
	Delivery           Delivery `json:"delivery"`
	IntegrationBranch  string   `json:"integration_branch"`
	ReleaseBranch      string   `json:"release_branch"`
	StagingOrigin      string   `json:"staging_origin"`
	ProductionOrigin   string   `json:"production_origin"`
	StagingWorkflow    string   `json:"staging_workflow"`
	ProductionWorkflow string   `json:"production_workflow"`
	// StagingLoginURL and ProductionLoginURL are the consumer's login
	// entries for the observation browser, one per environment. The
	// browser opens the entry with the session jar before every
	// observation and counts itself signed in once it rests on the
	// environment's own origin — so a console whose session lasts a day is
	// signed in again on each visit for as long as the identity provider
	// still recognises the jar. Optional: a public page needs none.
	StagingLoginURL    string `json:"staging_login_url,omitempty"`
	ProductionLoginURL string `json:"production_login_url,omitempty"`
	// ObservationLanguage is the language the observation browser asks the
	// destination's pages for (a BCP 47 tag such as "ja"). A console that
	// follows the browser's language renders its default wording to a
	// browser without one, and a promise about wording in another language
	// could never be seen. Optional.
	ObservationLanguage string `json:"observation_language,omitempty"`
	// Design is the destination's say over the design stage the reception
	// decides on for every change request (whether a change may skip the
	// design, and which words in a ticket mean the running system has to be
	// measured first). Optional: absent means design on, judged with the
	// framework's DefaultDesignTriggerWords.
	Design *DesignConfig `json:"design,omitempty"`
	// GitHub is the destination repository's observed delivery contract.
	// CLI destinations pin its default branch; web destinations also pin
	// merge settings, the exact workflows and required jobs, and
	// the staging digest-commit policy. These are the customer's observed
	// values, so they live here in configuration — an engine binary carries
	// no customer name (a fixed in-code contract table used to, which is
	// what stopped the engine from standing alone as a product).
	GitHub ConsumerGitHubContract `json:"github_contract"`
	Mode   ModeConfig             `json:"mode"`
	// Infrastructure is what this destination lets the engine create and
	// use outside the repository. A pointer so a destination that declares
	// none keeps its canonical form, and every digest bound to it, exactly
	// as it was.
	Infrastructure *InfrastructureConfig `json:"infrastructure,omitempty"`
}

// InfrastructureConfig is the destination's standing permission: which
// provider, in which region, under which provisioned credential, and which
// kinds of resource the engine may bring into existence to finish a
// request.
//
// It is a declaration, not a description of anything that exists. A kind
// listed here is one the engine may create when the request needs it; a
// kind left out is one it may not, and a request that needs it is carried
// as far as the means allow and reported with what was missing named.
// Nothing here is read as an instruction to create anything.
type InfrastructureConfig struct {
	// Provider is the service the resources live in, in that service's own
	// word for itself. The engine does not interpret it — it reaches the
	// provider through the credential's own tooling — so it is a label for
	// the record and for the person reading the report.
	Provider string `json:"provider"`
	// Region is where they are created, again in the provider's own word.
	Region string `json:"region,omitempty"`
	// Credential names the entry in the runtime configuration's
	// chain.credentials that reaches this provider. The two files are
	// loaded apart, so the name is checked where they meet rather than
	// here.
	Credential string `json:"credential,omitempty"`
	// Resources are the kinds the engine may create. Empty means it may
	// use what the credential reaches and create nothing.
	Resources []string `json:"resources,omitempty"`
	// NamingPrefix goes in front of every name the engine chooses, so that
	// what it made can be told apart afterwards from what was already
	// there.
	NamingPrefix string `json:"naming_prefix,omitempty"`
	// DeployWorkflows is the one means that is a policy rather than a name
	// in Resources, because what it permits is a file whose contents decide
	// what runs on this destination's own machines. A pointer, and absent
	// for every destination configured before it existed, so no digest
	// bound to such a configuration moves.
	DeployWorkflows *DeployWorkflowPolicy `json:"deploy_workflows,omitempty"`
}

var (
	// A provider, a region and a resource kind are all short identifiers in
	// somebody else's vocabulary; they end up in the record and in the
	// report, so they are held to a shape that prints as it stands.
	infrastructureWordPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	infrastructurePrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,30}$`)
)

// ValidateInfrastructure is the same check the setup runs before it writes
// a configuration, so a mistyped answer is named where it was written.
func ValidateInfrastructure(i InfrastructureConfig) error { return i.validate() }

// validate refuses a block that could not mean what it says.
func (i InfrastructureConfig) validate() error {
	if !infrastructureWordPattern.MatchString(i.Provider) {
		return errors.New("consumer infrastructure provider is invalid")
	}
	if i.Region != "" && !infrastructureWordPattern.MatchString(i.Region) {
		return errors.New("consumer infrastructure region is invalid")
	}
	if i.Credential != "" && !infrastructureWordPattern.MatchString(i.Credential) {
		return errors.New("consumer infrastructure credential name is invalid")
	}
	if len(i.Resources) > 32 {
		return errors.New("consumer infrastructure names too many resource kinds")
	}
	seen := make(map[string]bool, len(i.Resources))
	for _, kind := range i.Resources {
		if !infrastructureWordPattern.MatchString(kind) {
			return errors.New("consumer infrastructure resource kind is invalid")
		}
		if seen[kind] {
			return errors.New("consumer infrastructure names a resource kind twice")
		}
		seen[kind] = true
	}
	if i.NamingPrefix != "" && !infrastructurePrefixPattern.MatchString(i.NamingPrefix) {
		return errors.New("consumer infrastructure naming prefix is invalid")
	}
	if i.DeployWorkflows != nil {
		return i.DeployWorkflows.validate()
	}
	return nil
}

// MayCreate reports whether this destination allows a kind of resource to
// be brought into existence. A destination that declared no infrastructure
// allows none, which is every destination configured before this existed.
func (c ConsumerConfig) MayCreate(kind string) bool {
	if c.Infrastructure == nil {
		return false
	}
	for _, allowed := range c.Infrastructure.Resources {
		if allowed == kind {
			return true
		}
	}
	return false
}

// ConsumerGitHubContract mirrors githubapi.Contract as configuration. There
// is deliberately no discovery or fallback: every value is written down after
// being observed on the destination, and drift stops the run.
type ConsumerGitHubContract struct {
	DefaultBranch       string                `json:"default_branch"`
	MergeSettings       ConsumerMergeSettings `json:"merge_settings"`
	FeatureWorkflows    []ConsumerWorkflow    `json:"feature_workflows,omitempty"`
	StagingWorkflow     ConsumerWorkflow      `json:"staging_workflow"`
	ProductionWorkflows []ConsumerWorkflow    `json:"production_workflows"`
	StagingDigestCommit *ConsumerDigestCommit `json:"staging_digest_commit,omitempty"`
}

type ConsumerMergeSettings struct {
	AllowMergeCommit          bool   `json:"allow_merge_commit"`
	AllowSquashMerge          bool   `json:"allow_squash_merge"`
	AllowRebaseMerge          bool   `json:"allow_rebase_merge"`
	AllowAutoMerge            bool   `json:"allow_auto_merge"`
	AllowUpdateBranch         bool   `json:"allow_update_branch"`
	DeleteBranchOnMerge       bool   `json:"delete_branch_on_merge"`
	UseSquashPRTitleAsDefault bool   `json:"use_squash_pr_title_as_default"`
	SquashMergeCommitTitle    string `json:"squash_merge_commit_title"`
	SquashMergeCommitMessage  string `json:"squash_merge_commit_message"`
	MergeCommitTitle          string `json:"merge_commit_title"`
	MergeCommitMessage        string `json:"merge_commit_message"`
	WebCommitSignoffRequired  bool   `json:"web_commit_signoff_required"`
}

type ConsumerWorkflow struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	RequiredJobs []string `json:"required_jobs,omitempty"`
	// DeployPaths declares what this deployment reacts to, read the way a
	// workflow's own paths filter is read: an entry without glob characters
	// is a prefix ("docs/" covers everything under docs), "*" and "?" stay
	// inside one path segment, "**" spans segments wherever it appears
	// ("docs/**", "**/*.go", "**.js"), "[...]" is a class, and a trailing
	// "/" means "anything under such a directory". One reading differs from
	// GitHub's: "?" is exactly one character. Characters a delivered path
	// cannot carry (anything outside [A-Za-z0-9._/-] and the glob syntax)
	// are refused, so an entry cannot look declared while matching nothing.
	// When set and no delivered path falls under any entry, the runner records the
	// phase as not applicable instead of waiting for a run the destination
	// will not create. Empty means unknown, and the runner waits as before.
	DeployPaths []string `json:"deploy_paths,omitempty"`
}

// validateDeployPaths refuses every entry the matcher could not read as
// declared (see compileDeployPath): the config fails to load rather than
// carrying a scope that covers nothing.
func (w ConsumerWorkflow) validateDeployPaths() error {
	for _, pattern := range w.DeployPaths {
		if _, err := compileDeployPath(pattern); err != nil {
			return err
		}
	}
	return nil
}

type ConsumerDigestCommit struct {
	RequireDigestOnly  bool     `json:"require_digest_only"`
	ExactMessagePrefix string   `json:"exact_message_prefix"`
	ExactPaths         []string `json:"exact_paths"`
	ActorLogin         string   `json:"actor_login"`
}

func (w ConsumerWorkflow) contract() githubapi.WorkflowContract {
	if w.ID == 0 && w.Name == "" && w.Path == "" && len(w.RequiredJobs) == 0 {
		return githubapi.WorkflowContract{}
	}
	return githubapi.WorkflowContract{
		ID: w.ID, Name: w.Name, Path: w.Path, State: "active",
		RequiredJobs: append([]string(nil), w.RequiredJobs...),
	}
}

// Contract converts the configured destination contract into the form the
// delivery controller verifies against GitHub.
func (c ConsumerConfig) Contract() githubapi.Contract {
	feature := make([]githubapi.WorkflowContract, 0, len(c.GitHub.FeatureWorkflows))
	for _, workflow := range c.GitHub.FeatureWorkflows {
		feature = append(feature, workflow.contract())
	}
	production := make([]githubapi.WorkflowContract, 0, len(c.GitHub.ProductionWorkflows))
	for _, workflow := range c.GitHub.ProductionWorkflows {
		production = append(production, workflow.contract())
	}
	settings := c.GitHub.MergeSettings
	return githubapi.Contract{
		Kind:              c.Kind,
		IntegrationBranch: c.IntegrationBranch,
		ReleaseBranch:     c.ReleaseBranch,
		DefaultBranch:     c.GitHub.DefaultBranch,
		MergeSettings: githubapi.MergeSettings{
			AllowMergeCommit: settings.AllowMergeCommit, AllowSquashMerge: settings.AllowSquashMerge,
			AllowRebaseMerge: settings.AllowRebaseMerge, AllowAutoMerge: settings.AllowAutoMerge,
			AllowUpdateBranch: settings.AllowUpdateBranch, DeleteBranchOnMerge: settings.DeleteBranchOnMerge,
			UseSquashPRTitleAsDefault: settings.UseSquashPRTitleAsDefault,
			SquashMergeCommitTitle:    settings.SquashMergeCommitTitle,
			SquashMergeCommitMessage:  settings.SquashMergeCommitMessage,
			MergeCommitTitle:          settings.MergeCommitTitle,
			MergeCommitMessage:        settings.MergeCommitMessage,
			WebCommitSignoffRequired:  settings.WebCommitSignoffRequired,
		},
		FeatureWorkflows: feature, StagingWorkflow: c.GitHub.StagingWorkflow.contract(),
		ProductionWorkflows: production,
	}
}

// StagingDigestCommitPolicy is the configured staging digest-commit policy.
// A destination without one is held to the merge itself at the staging and
// promotion gates: the branch must sit on the deployed merge and no digest
// commit may be claimed.
func (c ConsumerConfig) StagingDigestCommitPolicy() githubapi.DigestCommitPolicy {
	if c.GitHub.StagingDigestCommit == nil {
		return githubapi.DigestCommitPolicy{}
	}
	policy := c.GitHub.StagingDigestCommit
	return githubapi.DigestCommitPolicy{
		Required: true, RequireDigestOnly: policy.RequireDigestOnly,
		ExactMessagePrefix: policy.ExactMessagePrefix,
		ExactPaths:         append([]string(nil), policy.ExactPaths...),
		ActorLogin:         policy.ActorLogin,
	}
}

// DeliveryTargetBranch is the branch changes are proposed against.
func (c ConsumerConfig) DeliveryTargetBranch() string {
	if c.DeliveryBranch != "" {
		return c.DeliveryBranch
	}
	return c.IntegrationBranch
}

const (
	// DesignDefaultOn is the recommended setting: every change request gets
	// a design unless the reception can show all four skip conditions hold.
	DesignDefaultOn = "on"
	// DesignDefaultOff turns the design stage off for the destination's
	// change requests regardless of those conditions.
	DesignDefaultOff = "off"

	maxDesignTriggerWords    = 64
	maxDesignTriggerWordSize = 64
)

// DefaultDesignTriggerWords is the framework's own trigger vocabulary: the
// words in a ticket that mean the running system has to be observed before
// a fix is designed. It applies to every destination that configures no
// vocabulary of its own, so a destination is never made to write one just
// to let a small, precisely stated change skip its design.
//
// A hit forces the design stage with no appeal, so the list is precision
// first: only words that almost never describe anything but an unobserved
// live symptom. Words that also appear in finished investigations ("root
// cause: X, fix: Y", 原因を特定済み, 調査を終えたので), in UI names (Logs
// page, latency / レイテンシ column, "in production builds"), in the names
// of techniques (遅延読み込み, 遅延する設定, 再現性のあるビルド, 不安定版) or
// inside other words (ダイアログに, カタログを, あたまに, catalogs, slowly) are
// left out; the AI proposer and checker judge needs_design independently
// of this list and catch a symptom said in other words. The Japanese
// entries match as substrings and therefore carry the particle or
// inflection that keeps them out of unrelated words; the English entries
// match as whole words (see ticketTriggerWord), so their inflections are
// listed, and "can't" is listed with both apostrophes (U+0027 and the
// U+2019 that macOS and Word substitute).
var DefaultDesignTriggerWords = []string{
	"が遅い", "遅くなった", "遅くなって", "遅くなり", "遅すぎ", "遅延が",
	"が重い", "重くなった", "重くなって", "重くなり", "重すぎ", "時々",
	"ときどき", "断続的", "が不安定", "稀に", "本番で", "本番環境",
	"本番のみ", "本番だけ", "原因不明", "原因は不明", "原因が分から", "原因がわから",
	"再現しない", "再現できない", "再現できず", "再現できません", "再現条件",
	"slow", "slower", "slowness", "sluggish", "intermittent", "intermittently",
	"flaky", "production only", "prod only", "on prod", "on production", "cannot reproduce",
	"can't reproduce", "can’t reproduce",
}

// DesignConfig is a destination's design-stage policy. Every key is optional
// and absent means: design on, the framework's default trigger vocabulary,
// investigation reports reviewed.
type DesignConfig struct {
	// Default is "on" or "off"; absent reads as "on".
	Default string `json:"default,omitempty"`
	// TriggerWords are the words in a ticket that mean the running system
	// has to be observed before a fix is designed (in the requesters' own
	// language: slowness, intermittence, production, logs, root cause,
	// investigation). Absent or empty means DefaultDesignTriggerWords; a
	// configured list replaces the default rather than extending it. An
	// entry of ASCII letters, digits, spaces and hyphens matches as a whole
	// word, case-insensitively; any other entry matches as a case-folded
	// substring (ticketTriggerWord).
	TriggerWords []string `json:"trigger_words,omitempty"`
	// ReviewInvestigation says whether an investigation-only report gets a
	// grounding review before it is posted; absent reads as true.
	ReviewInvestigation *bool `json:"review_investigation,omitempty"`
}

func (d *DesignConfig) validate() error {
	if d == nil {
		return nil
	}
	switch d.Default {
	case "", DesignDefaultOn, DesignDefaultOff:
	default:
		return errors.New("design default must be on or off")
	}
	if len(d.TriggerWords) > maxDesignTriggerWords {
		return errors.New("design trigger words are invalid")
	}
	seen := make(map[string]struct{}, len(d.TriggerWords))
	for _, word := range d.TriggerWords {
		if validatePlainText(word, maxDesignTriggerWordSize, false) != nil {
			return errors.New("design trigger word is invalid")
		}
		folded := strings.ToLower(word)
		if _, exists := seen[folded]; exists {
			return errors.New("design trigger words contain duplicates")
		}
		seen[folded] = struct{}{}
	}
	return nil
}

// DesignEnabled reports whether change requests to this destination get a
// design stage by default. Absent configuration means yes.
func (c ConsumerConfig) DesignEnabled() bool {
	return c.Design == nil || c.Design.Default != DesignDefaultOff
}

// DesignTriggerWords is the destination's own trigger vocabulary, nil when
// none is configured.
func (c ConsumerConfig) DesignTriggerWords() []string {
	if c.Design == nil {
		return nil
	}
	return append([]string(nil), c.Design.TriggerWords...)
}

// EffectiveDesignTriggerWords is the vocabulary the design rule and the
// reception prompts actually use: the destination's own when it configured
// one, DefaultDesignTriggerWords otherwise.
func (c ConsumerConfig) EffectiveDesignTriggerWords() []string {
	if words := c.DesignTriggerWords(); len(words) > 0 {
		return words
	}
	return append([]string(nil), DefaultDesignTriggerWords...)
}

// ReviewsInvestigation reports whether an investigation-only report is
// reviewed for its grounding before it is posted. Absent means yes.
func (c ConsumerConfig) ReviewsInvestigation() bool {
	return c.Design == nil || c.Design.ReviewInvestigation == nil || *c.Design.ReviewInvestigation
}

type ModeConfig struct {
	ID                     string   `json:"id"`
	AllowedFilePrefixes    []string `json:"allowed_file_prefixes"`
	ForbiddenCandidateText []string `json:"forbidden_candidate_text"`
	MaxFiles               int      `json:"max_files"`
	MaxFileBytes           int      `json:"max_file_bytes"`
	MaxTotalBytes          int      `json:"max_total_bytes"`
	MaxChangedLines        int      `json:"max_changed_lines"`
	MaxChangedBytes        int      `json:"max_changed_bytes"`
	// IgnoredByproducts are base file names the destination's toolchain is
	// known to drop inside the writable scope while ignored by git - a
	// lockfile from the wrong package manager above all. The ignored-file
	// guard tolerates exactly these names; everything else it still treats
	// as a deliverable the repository would silently swallow. Measured
	// live: an implementer ran npm in a pnpm repository, its
	// package-lock.json landed inside the scope, and a finished
	// implementation died at the tally.
	IgnoredByproducts []string `json:"ignored_byproducts,omitempty"`
	// Toolchain names the binaries whose observed versions are sealed into
	// the validation evidence before the verification commands run. Which
	// tools that is — node and pnpm, go, nothing at all — is the consumer's
	// stack, not the framework's.
	Toolchain              []ToolRequirement `json:"toolchain"`
	VerifyWorkingDirectory string            `json:"verify_working_directory"`
	InstallCommand         []string          `json:"install_command"`
	VerifyCommands         [][]string        `json:"verify_commands"`
}

// ToolRequirement pins one observed binary version. StripVPrefix accepts
// binaries that print v-prefixed versions (node prints v22.x).
type ToolRequirement struct {
	Binary       string `json:"binary"`
	Version      string `json:"version"`
	StripVPrefix bool   `json:"strip_v_prefix,omitempty"`
}

type ModelConfig struct {
	Implementer ModelEndpoint   `json:"implementer"`
	Reviewers   []ModelEndpoint `json:"reviewers"`
	Readiness   ReadinessModels `json:"readiness"`
	// Designer is the investigating designer's endpoint (the kernel calls it
	// directly; there is no agent). Optional until the design stages are
	// enabled. Like the implementer, at most one reviewer may share its
	// (base URL, model): a design judged only by its own author's model
	// family would be self-judgment.
	Designer *ModelEndpoint `json:"designer,omitempty"`
	// DesignReviewers, when present, are the design judges' endpoints: one
	// per reviewer id in Reviewers, all or none, so a design (or an
	// investigation report) can be judged by a heavier model or another
	// vendor than the candidate reviews use without moving those (design
	// doc §11, decision 3). The sealed DesignReview records the judge that
	// ran. Unset, the candidate reviewers judge designs as before.
	DesignReviewers []ModelEndpoint `json:"design_reviewers,omitempty"`
	// Arbiter, when present, is the endpoint that rules on a deadlock: a
	// delivery whose rounds have stopped moving is decided by this role
	// rather than handed back to the requester (§6 of the plan). Unset, the
	// reception assessor rules, which is the role already trusted to read a
	// ticket and say what it asks for. Optional, and omitted when empty, so
	// an existing configuration's canonical form is untouched.
	//
	// Candidates declared on this seat load and are held to the same rules
	// as any other seat's, and nothing moves the arbiter onto one yet: a
	// ruling is made in the attendant's own tick rather than on a card, so
	// there is no failed card for the ladder to climb from. An arbiter that
	// will not answer leaves the round to go on to the next one, which is
	// the delivery carrying itself rather than stopping. Moving the seat is
	// work for whoever gives the ruling a card of its own.
	Arbiter *ModelEndpoint `json:"arbiter,omitempty"`
	// ReceptionJudge, when present, is a decision model the reception may
	// put its own questions to: it answers with an option and a number
	// instead of prose, so the reception can weigh a second reading of a
	// request without a second prose seat. It is not a seat - it generates
	// nothing, reviews nothing, and its id never appears in a record of a
	// round - so it carries none of a seat's fields and takes part in none
	// of the different-vendor rules.
	//
	// Absent, and the reception behaves exactly as it did before this
	// existed. The field is omitted when empty, so a destination that says
	// nothing about it encodes - and therefore digests - as it always did,
	// and no delivery in flight is invalidated by the role being added.
	ReceptionJudge *ReceptionJudgeConfig `json:"reception_judge,omitempty"`
	// VendorHosts, when present, pins every declared vendor name to the hosts
	// its endpoints may be reached through. The different-vendor rules below
	// otherwise trust the vendor string as written: a config could call two
	// endpoints different vendors while pointing both anywhere. The table
	// rejects a vendor name it does not list and a base URL host not
	// registered for that vendor. Behind a shared gateway every vendor
	// legitimately maps to the same gateway host, and what runs beyond the
	// gateway stays unverifiable from here — the table's guarantee ends at
	// the connection target.
	VendorHosts map[string][]string `json:"vendor_hosts,omitempty"`
}

// ReadinessModels are the pre-generation gate models: a primary assessor and
// an adversarial checker that must come from a different vendor so the
// assessor cannot approve its own judgment family.
type ReadinessModels struct {
	Assessor ModelEndpoint `json:"assessor"`
	Checker  ModelEndpoint `json:"checker"`
}

// ReceptionJudgeConfig is where the reception's decision model is reached.
// It is deliberately not a ModelEndpoint: a decision model has no prose
// output, so an output allowance means nothing to it, and it has no lens,
// no effort and no candidates because nothing about it is a seat that could
// fail over to another.
//
// BaseURL is its own. The decisions service is not the chat-completions
// address the seats post to, so a role that inherited a gateway's base URL
// would reach a completions handler on every call and fail in a way that
// reads as the model being down. Left empty it is the service's published
// address; an operator who reaches it through something else names that.
type ReceptionJudgeConfig struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env"`
	// ProceedThreshold is how sure the judge has to be before the reception
	// settles its own questions instead of putting them to the requester.
	// It is a pointer so that a destination saying nothing is told apart
	// from one that wrote a zero: an omitted setting takes the measured
	// default, and a zero is refused rather than read as "always settle".
	//
	// Omitted is also how a destination's digest stays where it was, which
	// matters because the field sits inside a role that is itself optional.
	ProceedThreshold *float64 `json:"proceed_threshold,omitempty"`
}

// The bounds the threshold is held to. Below the floor the setting would be
// a number the measurement never covered, and there is no point in a judge
// consulted at less certainty than a coin toss; the ceiling is a judge that
// never settles anything, which is the honest way to turn the feature off
// without removing the role and losing the address with it.
const (
	MinReceptionProceedThreshold = 0.5
	MaxReceptionProceedThreshold = 1.0
)

// Threshold is the confidence this role settles questions at, filling in the
// default for a destination that named none.
func (r ReceptionJudgeConfig) Threshold() float64 {
	if r.ProceedThreshold == nil {
		return DefaultReceptionProceedThreshold
	}
	return *r.ProceedThreshold
}

// DecisionsBaseURL is the decisions service's published address, used when
// the role names none. It is stated here as well as in the client package so
// that a configuration can be validated without the client being built.
const DecisionsBaseURL = "https://openrouter.ai/api/alpha"

// Address is where this role is reached, filling in the default.
func (r ReceptionJudgeConfig) Address() string {
	if r.BaseURL == "" {
		return DecisionsBaseURL
	}
	return r.BaseURL
}

// validate holds the role to what a call needs: somewhere to post, a model
// to name, and a variable the key arrives in. The address is held to the
// same spelling rule the seats' addresses are, so one malformed URL is
// refused the same way wherever it appears.
func (r ReceptionJudgeConfig) validate() error {
	for _, value := range []string{r.Provider, r.Model} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") || len(value) > 128 {
			return errors.New("reception judge provider and model are invalid")
		}
	}
	if err := validateModelBaseURL(r.Address()); err != nil {
		return err
	}
	if !apiKeyEnvPattern.MatchString(r.APIKeyEnv) {
		return errors.New("reception judge api key environment name is invalid")
	}
	// A NaN is refused by the same comparison that refuses a number outside
	// the bounds: it fails both, and a threshold no confidence can reach or
	// miss would decide the feature's behaviour by which way the comparison
	// happened to be written.
	if r.ProceedThreshold != nil &&
		!(*r.ProceedThreshold >= MinReceptionProceedThreshold && *r.ProceedThreshold <= MaxReceptionProceedThreshold) {
		return errors.New("reception judge proceed threshold is outside 0.5 to 1.0")
	}
	return nil
}

// ReceptionJudgeRole reports the configured reception judge, and false when
// no role is configured. Callers branch on the boolean: without a role the
// reception asks nobody anything extra and runs exactly as before.
func (m ModelConfig) ReceptionJudgeRole() (ReceptionJudgeConfig, bool) {
	if m.ReceptionJudge == nil {
		return ReceptionJudgeConfig{}, false
	}
	return *m.ReceptionJudge, true
}

// ModelEndpoint names one OpenAI-compatible chat completions endpoint. The
// framework never holds a provider credential itself: BaseURL says where the
// consumer's gateway listens and APIKeyEnv names the environment variable the
// consumer injects the key through.
// ArbiterEndpoint is the seat that rules on a deadlocked delivery. A
// configuration that names no arbiter is ruled on by its reception
// assessor: the deadlock is a question about what the ticket asks for, and
// that is the role that already answers those.
func (m ModelConfig) ArbiterEndpoint() ModelEndpoint {
	if m.Arbiter != nil {
		return *m.Arbiter
	}
	return m.Readiness.Assessor
}

// DesignReviewerFor returns the endpoint that judges designs and
// investigation reports for reviewer id: the design judge when one is
// configured, else the candidate reviewer of that id.
func (m ModelConfig) DesignReviewerFor(id string) (ModelEndpoint, bool) {
	for _, judge := range m.DesignReviewers {
		if judge.ID == id {
			return judge, true
		}
	}
	for _, reviewer := range m.Reviewers {
		if reviewer.ID == id {
			return reviewer, true
		}
	}
	return ModelEndpoint{}, false
}

// DesignJudges lists the endpoints that judge designs: the design judges
// when configured, else the candidate reviewers.
func (m ModelConfig) DesignJudges() []ModelEndpoint {
	if len(m.DesignReviewers) > 0 {
		return m.DesignReviewers
	}
	return m.Reviewers
}

type ModelEndpoint struct {
	ID               string `json:"id"`
	Vendor           string `json:"vendor"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	APIKeyEnv        string `json:"api_key_env"`
	Lens             string `json:"lens,omitempty"`
	Effort           string `json:"effort,omitempty"`
	StructuredOutput bool   `json:"structured_output"`
	MaxOutputTokens  int32  `json:"max_output_tokens"`
	// DesignLens is the lens this reviewer judges a design or an
	// investigation report under, when the consumer wants its own wording.
	// Without one the first configured reviewer judges the evidence and
	// the second the approach (the framework's built-in lenses). Reviewers
	// only.
	DesignLens string `json:"design_lens,omitempty"`
	// Candidates are the other models that may take this seat when the one
	// above will not answer (seat.go). They are tried in order and each is
	// a whole endpoint: another vendor, another address, another key. The
	// field is omitted when empty, so a configuration written before seats
	// existed encodes — and therefore digests — exactly as it did.
	Candidates []ModelEndpoint `json:"candidates,omitempty"`
}

// MaxConfiguredOutputTokens is the ceiling of a model endpoint's
// max_output_tokens. Validate refuses a larger configuration, and a turn cut
// off at its allowance is asked again with more room only below it
// (converseTurn).
const MaxConfiguredOutputTokens = 32768

func LoadConfig(filename string) (Config, error) {
	var config Config
	if err := ReadJSONFile(filename, MaxConfigJSONBytes, &config); err != nil {
		return Config{}, errors.New("read worker config: invalid JSON input")
	}
	config.applyDeliveryDefault()
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate worker config: %w", err)
	}
	return config, nil
}

// applyDeliveryDefault fills in how far a destination's changes travel when
// the file does not say.
//
// Production, because the engine is meant to finish the job: a destination
// that only wants a proposal says so, rather than every destination having
// to ask for the rest. A CLI destination has no environment to reach, so
// its only honest default is the proposal.
//
// This changes no digest that exists. The field carries no omitempty, so
// every configuration that loads today already names a value and already
// hashes with it; only a file that omits it — refused outright until now —
// is affected at all.
func (c *Config) applyDeliveryDefault() {
	for index := range c.Consumers {
		if c.Consumers[index].Delivery != "" {
			continue
		}
		if c.Consumers[index].EffectiveKind() == "cli" {
			c.Consumers[index].Delivery = DeliverPullRequest
			continue
		}
		c.Consumers[index].Delivery = DeliverProduction
	}
}

// SHA256 returns the canonical digest of the validated configuration. The
// worker marshals the typed structure rather than hashing source JSON bytes so
// insignificant whitespace and object-key order cannot change the identity.
func (c Config) SHA256() (string, error) {
	if err := c.Validate(); err != nil {
		return "", errors.New("worker configuration is invalid")
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		return "", errors.New("worker configuration could not be encoded")
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// ForFinishedRun holds this configuration to the digest a run recorded while
// it was running, and returns it; the receiver is untouched.
//
// While a run is in flight the two are deliberately the same: if the
// destination's configuration moves underneath a run, the rules it was
// admitted under are no longer the rules, and finishing it under a mixture
// of both is worse than refusing it. Once the run has ended there is nothing
// left to admit — its pull request is already published — and holding its
// sealed records to today's digest only makes them unreadable. Reading a
// finished run that way is how a delivery whose pull request had been merged
// sat on the board asking for the merge for more than ten hours, while a run
// started after the change was noticed within seconds (live 2026-09-24).
func (c Config) ForFinishedRun(recorded string) (Config, error) {
	if !sha256Pattern.MatchString(recorded) {
		return Config{}, errors.New("recorded configuration digest is invalid")
	}
	c.finishedRunSHA256 = recorded
	return c, nil
}

// RunConfigSHA256 is the digest a run's sealed records must carry: the live
// configuration's own, unless this configuration was held to a finished run's
// recorded one. The configuration itself is checked either way, so a pinned
// one is no easier to pass than a live one.
func (c Config) RunConfigSHA256() (string, error) {
	if c.finishedRunSHA256 == "" {
		return c.SHA256()
	}
	if err := c.Validate(); err != nil {
		return "", errors.New("worker configuration is invalid")
	}
	return c.finishedRunSHA256, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != ConfigSchemaVersion {
		return errors.New("unsupported schema version")
	}
	if len(c.Consumers) < 1 || len(c.Consumers) > 8 {
		return errors.New("consumer count must be between 1 and 8")
	}
	repositories := make(map[string]struct{}, len(c.Consumers))
	identifiers := make(map[int64]struct{}, len(c.Consumers))
	for _, consumer := range c.Consumers {
		if err := consumer.validate(); err != nil {
			return err
		}
		if _, exists := repositories[consumer.Repository]; exists {
			return errors.New("consumer repositories contain duplicates")
		}
		repositories[consumer.Repository] = struct{}{}
		if _, exists := identifiers[consumer.RepositoryID]; exists {
			return errors.New("consumer repository ids contain duplicates")
		}
		identifiers[consumer.RepositoryID] = struct{}{}
	}
	if err := c.Models.validate(); err != nil {
		return err
	}
	if err := c.validateProbes(); err != nil {
		return err
	}
	if err := c.Agents.validate(); err != nil {
		return err
	}
	// Bindings are all or none: a partial set would silently judge the
	// unbound reviewers under the shared launch definition, and a forgotten
	// binding must be a loading failure, not a quiet substitution.
	if len(c.Agents.ReviewerAgents) > 0 && len(c.Agents.ReviewerAgents) != len(c.Models.Reviewers) {
		return errors.New("reviewer agents must cover every reviewer or none")
	}
	// A reviewer-agent binding must name a reviewer this configuration runs;
	// the two lists live apart, so the cross-check lives here.
	for _, entry := range c.Agents.ReviewerAgents {
		known := false
		for _, reviewer := range c.Models.Reviewers {
			if reviewer.ID == entry.ReviewerID {
				known = true
				break
			}
		}
		if !known {
			return errors.New("reviewer agent names a reviewer that is not configured")
		}
	}
	if len(c.Agents.DesignReviewerAgents) > 0 && len(c.Agents.DesignReviewerAgents) != len(c.Models.Reviewers) {
		return errors.New("design reviewer agents must cover every reviewer or none")
	}
	for _, entry := range c.Agents.DesignReviewerAgents {
		known := false
		for _, reviewer := range c.Models.Reviewers {
			if reviewer.ID == entry.ReviewerID {
				known = true
				break
			}
		}
		if !known {
			return errors.New("design reviewer agent names a reviewer that is not configured")
		}
	}
	// The endpoints and the launches come together: the model that runs is
	// the launch's profile, so judge endpoints alone would seal a model
	// that did not run, and judge launches alone would run under the
	// candidate reviewers' declared endpoints.
	if (len(c.Models.DesignReviewers) > 0) != (len(c.Agents.DesignReviewerAgents) > 0) {
		return errors.New("design reviewers and design reviewer agents must be configured together")
	}
	if err := c.validateSeatLaunches(); err != nil {
		return err
	}
	if c.MaxStages < 1 || c.MaxStages > 5 {
		return errors.New("max_stages must be between 1 and 5")
	}
	if c.DesignMaxRounds < 0 || c.DesignMaxRounds > 10 {
		return errors.New("design_max_rounds must be between 1 and 10")
	}
	// Zero is the whole point of the round limit, so the range starts below
	// one: an operator who wants a delivery to stop after so many rounds
	// says so, and everyone else gets a delivery that runs until it is done.
	if c.MaxRounds < 0 || c.MaxRounds > StageCeiling {
		return fmt.Errorf("max_rounds must be between 0 and %d", StageCeiling)
	}
	if c.StagnationRepeatRounds < 0 || c.StagnationRepeatRounds > 10 {
		return errors.New("stagnation_repeat_rounds must be between 0 and 10")
	}
	if c.AnswerKnowledge != nil {
		if err := c.AnswerKnowledge.validate(); err != nil {
			return err
		}
	}
	if err := c.validateReception(); err != nil {
		return err
	}
	return nil
}

// EffectiveKind preserves the web contract for configurations predating kind.
func (c ConsumerConfig) EffectiveKind() string {
	if c.Kind == "" {
		return "web"
	}
	return c.Kind
}

func (c ConsumerConfig) validate() error {
	if !repositoryPattern.MatchString(c.Repository) || c.RepositoryID <= 0 {
		return errors.New("consumer repository is invalid")
	}
	if c.Infrastructure != nil {
		if err := c.Infrastructure.validate(); err != nil {
			return err
		}
	}
	if c.Description != "" && validatePlainText(c.Description, 256, false) != nil {
		return errors.New("consumer description is invalid")
	}
	for _, workflow := range append([]ConsumerWorkflow{c.GitHub.StagingWorkflow}, c.GitHub.ProductionWorkflows...) {
		if err := workflow.validateDeployPaths(); err != nil {
			return err
		}
	}
	for _, workflow := range c.GitHub.FeatureWorkflows {
		// Nothing reads a feature workflow's scope: the feature CI is waited
		// for by its required jobs, never skipped. Accepting the field there
		// would look like a declaration that does something.
		if len(workflow.DeployPaths) > 0 {
			return errors.New("consumer feature workflow deploy_paths is not read; declare it on the staging or production workflow")
		}
	}
	switch c.EffectiveKind() {
	case "cli":
		return c.validateCLI()
	case "web":
	default:
		return errors.New("consumer kind is invalid")
	}
	if !validBranch(c.IntegrationBranch) || !validBranch(c.ReleaseBranch) || c.IntegrationBranch == c.ReleaseBranch {
		return errors.New("consumer branches are invalid")
	}
	if !c.Delivery.valid() {
		return errors.New("consumer delivery is invalid")
	}
	if c.DeliveryBranch != "" && (!validBranch(c.DeliveryBranch) || c.DeliveryBranch == c.ReleaseBranch) {
		return errors.New("consumer delivery branch is invalid")
	}
	if err := validateOrigin(c.StagingOrigin); err != nil {
		return fmt.Errorf("staging origin: %w", err)
	}
	if err := validateOrigin(c.ProductionOrigin); err != nil {
		return fmt.Errorf("production origin: %w", err)
	}
	if c.StagingOrigin == c.ProductionOrigin {
		return errors.New("consumer origins must differ")
	}
	if c.StagingLoginURL != "" && !ValidLoginURL(c.StagingLoginURL) {
		return errors.New("consumer staging login url is invalid")
	}
	if c.ProductionLoginURL != "" && !ValidLoginURL(c.ProductionLoginURL) {
		return errors.New("consumer production login url is invalid")
	}
	if c.StagingLoginURL != "" && c.StagingLoginURL == c.ProductionLoginURL {
		// One entry for both environments signs the production observer
		// in to staging, and every production report ends unjudged.
		return errors.New("consumer login urls must differ")
	}
	if c.ObservationLanguage != "" && !languageTagPattern.MatchString(c.ObservationLanguage) {
		return errors.New("consumer observation language is invalid")
	}
	if err := c.Design.validate(); err != nil {
		return err
	}
	if !validWorkflowFilename(c.StagingWorkflow) || !validWorkflowFilename(c.ProductionWorkflow) || c.StagingWorkflow == c.ProductionWorkflow {
		return errors.New("consumer workflows are invalid")
	}
	if err := c.Contract().Validate(); err != nil {
		return fmt.Errorf("consumer github contract: %w", err)
	}
	// The short workflow filename fields and the full contract entries must
	// describe the same workflows, or half the checks watch the wrong one.
	if path.Base(c.GitHub.StagingWorkflow.Path) != c.StagingWorkflow ||
		len(c.GitHub.ProductionWorkflows) == 0 ||
		path.Base(c.GitHub.ProductionWorkflows[0].Path) != c.ProductionWorkflow {
		return errors.New("consumer github contract does not match the named workflows")
	}
	if c.GitHub.StagingDigestCommit != nil {
		policy := c.GitHub.StagingDigestCommit
		if policy.ExactMessagePrefix == "" || len(policy.ExactPaths) == 0 || policy.ActorLogin == "" {
			return errors.New("consumer staging digest policy is incomplete")
		}
	}
	if err := c.Mode.validate(); err != nil {
		return err
	}
	return nil
}

// validateCLI retains the shared repository, branch, verification and writable
// scope contract while refusing settings for stages a CLI never runs.
func (c ConsumerConfig) validateCLI() error {
	if c.Delivery != DeliverPullRequest {
		return errors.New("cli consumer delivery must be pull_request")
	}
	if !validBranch(c.IntegrationBranch) || (c.DeliveryBranch != "" && !validBranch(c.DeliveryBranch)) {
		return errors.New("consumer branches are invalid")
	}
	if c.ReleaseBranch != "" || c.StagingOrigin != "" || c.ProductionOrigin != "" ||
		c.StagingLoginURL != "" || c.ProductionLoginURL != "" || c.ObservationLanguage != "" ||
		c.StagingWorkflow != "" || c.ProductionWorkflow != "" || c.GitHub.StagingDigestCommit != nil {
		return errors.New("cli consumer cannot configure web observation or release")
	}
	if err := c.Contract().Validate(); err != nil {
		return fmt.Errorf("consumer github contract: %w", err)
	}
	if err := c.Design.validate(); err != nil {
		return err
	}
	return c.Mode.validate()
}

func (c ModeConfig) validate() error {
	if !identifierPattern.MatchString(c.ID) {
		return errors.New("mode id is invalid")
	}
	if c.MaxFiles < 1 || c.MaxFiles > 16 || c.MaxFileBytes < 1 || c.MaxFileBytes > 512*1024 ||
		c.MaxTotalBytes < c.MaxFileBytes || c.MaxTotalBytes > 1024*1024 ||
		c.MaxChangedLines < 1 || c.MaxChangedLines > 4000 ||
		c.MaxChangedBytes < 1 || c.MaxChangedBytes > c.MaxTotalBytes*2 {
		return errors.New("mode file limits are invalid")
	}
	if len(c.IgnoredByproducts) > 16 {
		return errors.New("mode ignored byproducts are invalid")
	}
	for _, name := range c.IgnoredByproducts {
		if name == "" || len(name) > 128 || strings.ContainsAny(name, "/\\\r\n\x00") || name != strings.TrimSpace(name) {
			return errors.New("mode ignored byproduct name is invalid")
		}
	}
	// The list is not capped: a repository names as many top-level directories
	// as it has, and an arbitrary ceiling only forces a consumer to leave real
	// directories out. A change that lands somewhere unlisted is still refused,
	// so the list's job is to be complete, not short. At least one entry is
	// still required — an empty list would mean "anywhere", which has to be
	// stated by naming the directories rather than by omission.
	if len(c.AllowedFilePrefixes) == 0 {
		return errors.New("mode file prefixes are invalid")
	}
	prefixes := make(map[string]struct{}, len(c.AllowedFilePrefixes))
	for _, prefix := range c.AllowedFilePrefixes {
		if !validFilePrefix(prefix) {
			return errors.New("mode file prefix is invalid")
		}
		if _, exists := prefixes[prefix]; exists {
			return errors.New("mode file prefixes contain duplicates")
		}
		prefixes[prefix] = struct{}{}
	}
	if len(c.ForbiddenCandidateText) == 0 || len(c.ForbiddenCandidateText) > 32 {
		return errors.New("forbidden candidate text is invalid")
	}
	forbidden := make(map[string]struct{}, len(c.ForbiddenCandidateText))
	for _, value := range c.ForbiddenCandidateText {
		folded := strings.ToLower(value)
		if value == "" || strings.TrimSpace(value) != value || len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("forbidden candidate text is invalid")
		}
		if _, exists := forbidden[folded]; exists {
			return errors.New("forbidden candidate text contains duplicates")
		}
		forbidden[folded] = struct{}{}
	}
	if len(c.Toolchain) > 4 {
		return errors.New("mode toolchain is invalid")
	}
	binaries := make(map[string]struct{}, len(c.Toolchain))
	for _, tool := range c.Toolchain {
		if !identifierPattern.MatchString(tool.Binary) || !versionPattern.MatchString(tool.Version) {
			return errors.New("mode toolchain is invalid")
		}
		if _, exists := binaries[tool.Binary]; exists {
			return errors.New("mode toolchain contains duplicates")
		}
		binaries[tool.Binary] = struct{}{}
	}
	if !validRelativeDirectory(c.VerifyWorkingDirectory) || !validCommand(c.InstallCommand) ||
		len(c.VerifyCommands) == 0 || len(c.VerifyCommands) > 4 {
		return errors.New("mode verification configuration is invalid")
	}
	for _, command := range c.VerifyCommands {
		if !validCommand(command) {
			return errors.New("mode verification command is invalid")
		}
	}
	return nil
}

func validCommand(command []string) bool {
	if len(command) == 0 || len(command) > 16 {
		return false
	}
	for _, argument := range command {
		if argument == "" || len(argument) > 256 || strings.ContainsAny(argument, "\r\n\x00") {
			return false
		}
	}
	return true
}

func (c ModelConfig) validate() error {
	if err := c.Implementer.validate(false); err != nil {
		return fmt.Errorf("implementer: %w", err)
	}
	// One judge is a legal configuration. A second opinion on a change only
	// pays for itself where being wrong is expensive; where it is not, the
	// two judges have to agree before anything ships, and two live
	// deliveries died deadlocked on 2026-09-17 over a judgement call a
	// single model settles in one pass.
	if len(c.Reviewers) < 1 || len(c.Reviewers) > 4 {
		return errors.New("reviewer count must be between 1 and 4")
	}
	ids := map[string]struct{}{c.Implementer.ID: {}}
	vendors := make(map[string]struct{}, len(c.Reviewers))
	models := make(map[string]struct{}, len(c.Reviewers))
	implementerModelKey := strings.ToLower(c.Implementer.BaseURL + "\x00" + c.Implementer.Model)
	sharedWithImplementer := 0
	reviewerIDs := make(map[string]struct{}, len(c.Reviewers))
	for _, reviewer := range c.Reviewers {
		if err := reviewer.validate(true); err != nil {
			return fmt.Errorf("reviewer: %w", err)
		}
		if _, exists := ids[reviewer.ID]; exists {
			return errors.New("reviewer ids contain duplicates")
		}
		ids[reviewer.ID] = struct{}{}
		reviewerIDs[reviewer.ID] = struct{}{}
		vendors[strings.ToLower(reviewer.Vendor)] = struct{}{}
		modelKey := strings.ToLower(reviewer.BaseURL + "\x00" + reviewer.Model)
		// One reviewer may run the implementer's own endpoint and model under
		// its own lens; a second would leave no reviewer the implementer's
		// model family cannot influence. The check is stated on the
		// implementer pair, not left to the duplicate rule below, so the
		// refusal names the actual problem.
		if modelKey == implementerModelKey {
			sharedWithImplementer++
			if sharedWithImplementer > 1 {
				return errors.New("at most one reviewer may share the implementer endpoint and model")
			}
		}
		if _, exists := models[modelKey]; exists {
			return errors.New("reviewer models contain duplicates")
		}
		models[modelKey] = struct{}{}
	}
	// Two judges must not share a vendor, or one vendor's blind spot is
	// every judge's blind spot. A single judge has no one to differ from,
	// and the rule has nothing to say about it.
	if len(c.Reviewers) > 1 && len(vendors) < 2 {
		return errors.New("reviewers must use at least two vendors")
	}
	if c.Designer != nil {
		if err := c.Designer.validate(false); err != nil {
			return fmt.Errorf("designer: %w", err)
		}
		if _, exists := ids[c.Designer.ID]; exists {
			return errors.New("designer id duplicates another model id")
		}
		ids[c.Designer.ID] = struct{}{}
		designerModelKey := strings.ToLower(c.Designer.BaseURL + "\x00" + c.Designer.Model)
		sharedWithDesigner := 0
		for _, reviewer := range c.Reviewers {
			if strings.ToLower(reviewer.BaseURL+"\x00"+reviewer.Model) == designerModelKey {
				sharedWithDesigner++
			}
		}
		if sharedWithDesigner > 1 {
			return errors.New("at most one reviewer may share the designer endpoint and model")
		}
	}
	if len(c.DesignReviewers) > 0 {
		// All or none, and the same ids: a design judge stands in for the
		// reviewer of the same letter, so a missing one would silently be
		// the candidate reviewer, and a stray id would judge nothing.
		if len(c.DesignReviewers) != len(c.Reviewers) {
			return errors.New("design reviewers must cover every reviewer or none")
		}
		judgeIDs := make(map[string]struct{}, len(c.DesignReviewers))
		judgeVendors := make(map[string]struct{}, len(c.DesignReviewers))
		judgeModels := make(map[string]struct{}, len(c.DesignReviewers))
		for _, judge := range c.DesignReviewers {
			// A judge carries no candidate-review lens; one given is kept to
			// the reviewer's shape, none is fine, and a design lens is
			// allowed either way (it applies when a caller passes no letter).
			if err := judge.validateAs(judge.Lens != "", true); err != nil {
				return fmt.Errorf("design reviewer: %w", err)
			}
			if _, exists := judgeIDs[judge.ID]; exists {
				return errors.New("design reviewer ids contain duplicates")
			}
			judgeIDs[judge.ID] = struct{}{}
			judgeVendors[strings.ToLower(judge.Vendor)] = struct{}{}
			if _, known := reviewerIDs[judge.ID]; !known {
				return errors.New("design reviewer names a reviewer that is not configured")
			}
			// No two judges on one (base URL, model): with two judges this is
			// also what keeps the designer's own model to one judge at most —
			// the same self-judgment bound the candidate reviewers carry.
			modelKey := strings.ToLower(judge.BaseURL + "\x00" + judge.Model)
			if _, exists := judgeModels[modelKey]; exists {
				return errors.New("design reviewer models contain duplicates")
			}
			judgeModels[modelKey] = struct{}{}
		}
		if len(judgeVendors) < 2 {
			return errors.New("design reviewers must use at least two vendors")
		}
	}
	if err := c.Readiness.Assessor.validate(false); err != nil {
		return fmt.Errorf("readiness assessor: %w", err)
	}
	if err := c.Readiness.Checker.validate(true); err != nil {
		return fmt.Errorf("readiness checker: %w", err)
	}
	for _, id := range []string{c.Readiness.Assessor.ID, c.Readiness.Checker.ID} {
		if _, exists := ids[id]; exists {
			return errors.New("readiness model ids contain duplicates")
		}
		ids[id] = struct{}{}
	}
	// The arbiter is a seat like the others: it spends a model turn, its id
	// names it in the records, and a delivery with two seats under one name
	// cannot say which of them answered.
	if c.Arbiter != nil {
		if err := c.Arbiter.validate(false); err != nil {
			return fmt.Errorf("arbiter: %w", err)
		}
		if _, exists := ids[c.Arbiter.ID]; exists {
			return errors.New("arbiter model id duplicates another seat")
		}
		ids[c.Arbiter.ID] = struct{}{}
	}
	if strings.EqualFold(c.Readiness.Assessor.Vendor, c.Readiness.Checker.Vendor) {
		return errors.New("readiness assessor and checker must use different vendors")
	}
	// The reception judge is checked but takes no part in the rules above:
	// it holds no id, so it cannot collide with a seat's, and it answers no
	// prose, so the different-vendor rules that keep one vendor from being
	// every judge have nothing to say about it.
	if c.ReceptionJudge != nil {
		if err := c.ReceptionJudge.validate(); err != nil {
			return fmt.Errorf("reception judge: %w", err)
		}
	}
	if c.VendorHosts != nil {
		if err := validateVendorHosts(c.VendorHosts); err != nil {
			return err
		}
		seats := append([]ModelEndpoint{c.Implementer, c.Readiness.Assessor, c.Readiness.Checker}, c.Reviewers...)
		seats = append(seats, c.DesignReviewers...)
		if c.Arbiter != nil {
			seats = append(seats, *c.Arbiter)
		}
		// Every occupant, not only the configured one. The table is what
		// stops a vendor name from pointing anywhere it likes, and a seat
		// that could be moved onto an unregistered host would be a way
		// round it that opens the moment the first model fails.
		for _, seat := range seats {
			for _, endpoint := range seat.Seat() {
				if err := vendorHostMatch(c.VendorHosts, endpoint); err != nil {
					return fmt.Errorf("%s: %w", endpoint.ID, err)
				}
			}
		}
	}
	return nil
}

func validateVendorHosts(table map[string][]string) error {
	if len(table) == 0 || len(table) > 16 {
		return errors.New("vendor host table is invalid")
	}
	for vendor, hosts := range table {
		// Keys are the canonical lowercase form of the vendor names the
		// endpoints declare, matched case-insensitively like every other
		// vendor comparison in this file.
		if vendor == "" || len(vendor) > 64 || vendor != strings.ToLower(vendor) ||
			strings.TrimSpace(vendor) != vendor || strings.ContainsAny(vendor, "\r\n\x00") {
			return errors.New("vendor host table vendor name is invalid")
		}
		if len(hosts) == 0 || len(hosts) > 8 {
			return errors.New("vendor host table hosts are invalid")
		}
		seen := make(map[string]struct{}, len(hosts))
		for _, host := range hosts {
			if !vendorHostPattern.MatchString(host) || strings.Contains(host, "..") {
				return errors.New("vendor host table host is invalid")
			}
			if _, exists := seen[host]; exists {
				return errors.New("vendor host table hosts contain duplicates")
			}
			seen[host] = struct{}{}
		}
	}
	return nil
}

// vendorHostMatch refuses an endpoint whose declared vendor is absent from
// the table or whose base URL host is not registered for it. The port is not
// part of the identity; the hostname is.
func vendorHostMatch(table map[string][]string, endpoint ModelEndpoint) error {
	hosts, exists := table[strings.ToLower(endpoint.Vendor)]
	if !exists {
		return errors.New("model vendor has no vendor host table entry")
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || !slices.Contains(hosts, parsed.Hostname()) {
		return errors.New("model base url host is not registered for its vendor")
	}
	return nil
}

// ValidToolSHA validates the immutable external worker/workflow revision bound
// into every artifact. It deliberately accepts only a full Git object ID.
func ValidToolSHA(value string) bool {
	return commitPattern.MatchString(value)
}

// ValidateModelEndpoint re-checks one endpoint at a call boundary, for callers
// that received it outside a validated Config.
func ValidateModelEndpoint(m ModelEndpoint) error {
	return m.validate(m.Lens != "")
}

func (m ModelEndpoint) validate(reviewer bool) error { return m.validateAs(reviewer, false) }

// validateAs is validate with the design judge's allowance: a judge may
// carry no candidate-review lens and still hold a design lens.
func (m ModelEndpoint) validateAs(reviewer, judge bool) error {
	if !identifierPattern.MatchString(m.ID) || m.Vendor == "" || m.Model == "" ||
		strings.TrimSpace(m.Vendor) != m.Vendor || strings.TrimSpace(m.Model) != m.Model ||
		strings.ContainsAny(m.Vendor+m.Model, "\r\n\x00") || m.MaxOutputTokens < 128 || m.MaxOutputTokens > MaxConfiguredOutputTokens {
		return errors.New("model endpoint is invalid")
	}
	if err := validateModelBaseURL(m.BaseURL); err != nil {
		return err
	}
	if !apiKeyEnvPattern.MatchString(m.APIKeyEnv) {
		return errors.New("model api key environment name is invalid")
	}
	if reviewer && !validLens(m.Lens) {
		return errors.New("reviewer lens is invalid")
	}
	if !reviewer && m.Lens != "" {
		return errors.New("implementer lens must be empty")
	}
	if m.DesignLens != "" && ((!reviewer && !judge) || !validLens(m.DesignLens)) {
		return errors.New("reviewer design lens is invalid")
	}
	switch m.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("model effort is invalid")
	}
	return m.validateCandidates(reviewer, judge)
}

func validBranch(value string) bool {
	return branchPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//") &&
		!strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, ".lock")
}

func validWorkflowFilename(value string) bool {
	return value != "" && path.Base(value) == value && (strings.HasSuffix(value, ".yml") || strings.HasSuffix(value, ".yaml")) &&
		!strings.ContainsAny(value, "\r\n\x00")
}

func validFilePrefix(value string) bool {
	if strings.HasSuffix(value, "/") {
		return validRelativePath(strings.TrimSuffix(value, "/"))
	}
	return validRelativePath(value) && !strings.Contains(value, "/") && !hasHiddenComponent(value)
}

// validRelativeDirectory additionally accepts "." because a repository whose
// verification runs at its root (a Go module, most single-language services)
// has no subdirectory to name.
func validRelativeDirectory(value string) bool {
	return value == "." || validRelativePath(value)
}

func validRelativePath(value string) bool {
	return relativePathPattern.MatchString(value) && value == path.Clean(value) && value != "." && !strings.HasPrefix(value, "/") &&
		!strings.HasPrefix(value, "../") && !strings.Contains(value, "\\") && !strings.ContainsAny(value, "\r\n\x00")
}

// hasHiddenComponent reports whether any path element is dotted, which keeps
// repository machinery and secret files out of the writable scope wherever a
// path is checked against it.
func hasHiddenComponent(candidate string) bool {
	for _, element := range strings.Split(candidate, "/") {
		if strings.HasPrefix(element, ".") {
			return true
		}
	}
	return false
}

// validateModelBaseURL accepts an https URL with an optional path prefix (for
// example https://gateway.example.com/api/v1) because OpenAI-compatible
// gateways commonly mount the API under a path. One spelling per endpoint:
// the duplicate-reviewer and shared-with-implementer rules compare base URLs
// as strings, so a second spelling of the same endpoint (an explicit :443, a
// dot-segment in the path) would count as a different one and hollow both
// rules out — measured live by an adversarial probe. Hence no port at all
// (the https default is the only spelling) and only already-clean paths.
func validateModelBaseURL(value string) error {
	if len(value) > 256 || strings.HasSuffix(value, "/") {
		return errors.New("model base url is invalid")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		strings.Contains(parsed.Host, ":") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != parsed.EscapedPath() ||
		(parsed.Path != "" && parsed.Path != path.Clean(parsed.Path)) ||
		strings.Contains(parsed.Path, "//") || strings.ContainsAny(value, "\r\n\x00 ") {
		return errors.New("model base url is invalid")
	}
	if parsed.Hostname() != strings.ToLower(parsed.Hostname()) {
		return errors.New("model base url hostname must be lowercase")
	}
	return nil
}

func validateOrigin(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Port() != "" ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("must be an https origin without path, query, userinfo, or port")
	}
	if !asciiLowercaseHost(parsed.Hostname()) {
		// The browser reports an internationalised host in punycode, which
		// a landing check against the written origin would never match.
		return errors.New("hostname must be lowercase ASCII")
	}
	return nil
}

// languageTagPattern is the shape of a BCP 47 tag the browser accepts on
// its command line: a language subtag and optional dashed subtags.
var languageTagPattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,8})*$`)

// ValidLanguageTag reports whether a value may be handed to the browser as
// the language to ask pages for. The lenient readers of the consumer
// configuration (the attendant's probe, the runner's observation) apply
// it too, so an unvalidated value never reaches a command line.
func ValidLanguageTag(value string) bool {
	return languageTagPattern.MatchString(value)
}

// ValidLoginURL accepts an absolute https link with a lowercase ASCII host
// (the browser reports internationalised hosts in punycode, which a
// landing check would never match) and neither userinfo, port, nor
// fragment. A query is allowed — a login entry commonly carries where to
// return to — but no whitespace or control characters anywhere.
func ValidLoginURL(raw string) bool {
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\x00\r\n\t ") {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Port() == "" &&
		asciiLowercaseHost(parsed.Hostname()) && strings.HasPrefix(parsed.Path, "/") &&
		parsed.Fragment == "" && parsed.String() == raw
}

func asciiLowercaseHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		if r > 0x7e || r < 0x21 || (r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// Origin is the environment's observation origin.
func (c ConsumerConfig) Origin(environment string) string {
	if environment == "production" {
		return c.ProductionOrigin
	}
	return c.StagingOrigin
}

// LoginURL is the environment's login entry, empty when the consumer has
// none for it.
func (c ConsumerConfig) LoginURL(environment string) string {
	if environment == "production" {
		return c.ProductionLoginURL
	}
	return c.StagingLoginURL
}

func allowedPath(filename string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool {
		return filename == prefix || strings.HasSuffix(prefix, "/") && strings.HasPrefix(filename, prefix)
	})
}

// validateProbes checks the investigating designer's catalogue the way the
// kernel will resolve it, and keeps every consumer's login entry — the
// identity provider that rotates the session cookie on use — out of the
// http probes' hosts.
func (c Config) validateProbes() error {
	if len(c.Probes) == 0 {
		return nil
	}
	catalog, err := probe.NewCatalog(c.Probes)
	if err != nil {
		return fmt.Errorf("probes: %w", err)
	}
	var loginHosts []string
	for _, consumer := range c.Consumers {
		for _, entry := range []string{consumer.StagingLoginURL, consumer.ProductionLoginURL} {
			if entry == "" {
				continue
			}
			if parsed, err := url.Parse(entry); err == nil && parsed.Host != "" {
				loginHosts = append(loginHosts, strings.ToLower(parsed.Host))
			}
		}
	}
	if err := catalog.ForbidHosts(loginHosts); err != nil {
		return fmt.Errorf("probes: %w (the identity provider rotates the session cookie on every use)", err)
	}
	return nil
}

// ProbeCatalog is the validated catalogue, built-ins included.
func (c Config) ProbeCatalog() (probe.Catalog, error) {
	return probe.NewCatalog(c.Probes)
}

func validLens(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 512 && !strings.ContainsAny(value, "\r\n\x00")
}
