package worker

import (
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
	identifierPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,63}$`)
	repositoryPattern       = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchPattern           = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	versionPattern          = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+){0,2}$`)
	sha256Pattern           = regexp.MustCompile(`^[a-f0-9]{64}$`)
	commitPattern           = regexp.MustCompile(`^[a-f0-9]{40}$`)
	deliveryPattern         = regexp.MustCompile(`^delivery_[a-f0-9]{32}$`)
	runIDPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{7,127}$`)
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
	// AnswerKnowledge names where adopted answers are preserved, if anywhere.
	AnswerKnowledge *AnswerKnowledgeConfig `json:"answer_knowledge,omitempty"`
}

// AgentSet names the coding agents the framework runs: one that implements the
// change by working in the repository, and one that reviews the result. They
// are separate programs with separate credentials so the reviewer cannot be
// the author of what it reviews.
type AgentSet struct {
	Implementer AgentConfig `json:"implementer"`
	Reviewer    AgentConfig `json:"reviewer"`
}

func (a AgentSet) validate() error {
	if err := a.Implementer.validate(); err != nil {
		return fmt.Errorf("implementer agent: %w", err)
	}
	if err := a.Reviewer.validate(); err != nil {
		return fmt.Errorf("reviewer agent: %w", err)
	}
	if a.Implementer.ID == a.Reviewer.ID {
		return errors.New("agent ids must differ")
	}
	if a.Implementer.Command == a.Reviewer.Command {
		return errors.New("the reviewer must not be the same program as the implementer")
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
	default:
		return AgentConfig{}, errors.New("agent run names an agent that is not configured")
	}
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
	// GitHub is the destination repository's observed delivery contract:
	// branches, merge settings, the exact workflows and required jobs, and
	// the staging digest-commit policy. These are the customer's observed
	// values, so they live here in configuration — an engine binary carries
	// no customer name (a fixed in-code contract table used to, which is
	// what stopped the engine from standing alone as a product).
	GitHub ConsumerGitHubContract `json:"github_contract"`
	Mode   ModeConfig             `json:"mode"`
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
}

type ConsumerDigestCommit struct {
	RequireDigestOnly  bool     `json:"require_digest_only"`
	ExactMessagePrefix string   `json:"exact_message_prefix"`
	ExactPaths         []string `json:"exact_paths"`
	ActorLogin         string   `json:"actor_login"`
}

func (w ConsumerWorkflow) contract() githubapi.WorkflowContract {
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
// A destination without one fails the staging digest checks closed.
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

// ModelEndpoint names one OpenAI-compatible chat completions endpoint. The
// framework never holds a provider credential itself: BaseURL says where the
// consumer's gateway listens and APIKeyEnv names the environment variable the
// consumer injects the key through.
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
}

func LoadConfig(filename string) (Config, error) {
	var config Config
	if err := ReadJSONFile(filename, MaxConfigJSONBytes, &config); err != nil {
		return Config{}, errors.New("read worker config: invalid JSON input")
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate worker config: %w", err)
	}
	return config, nil
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
	if err := c.Agents.validate(); err != nil {
		return err
	}
	if c.MaxStages < 1 || c.MaxStages > 5 {
		return errors.New("max_stages must be between 1 and 5")
	}
	if c.AnswerKnowledge != nil {
		if err := c.AnswerKnowledge.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c ConsumerConfig) validate() error {
	if !repositoryPattern.MatchString(c.Repository) || c.RepositoryID <= 0 {
		return errors.New("consumer repository is invalid")
	}
	if c.Description != "" && validatePlainText(c.Description, 256, false) != nil {
		return errors.New("consumer description is invalid")
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
	if len(c.AllowedFilePrefixes) == 0 || len(c.AllowedFilePrefixes) > 8 {
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
	if len(c.Reviewers) < 2 || len(c.Reviewers) > 4 {
		return errors.New("reviewer count must be between 2 and 4")
	}
	ids := map[string]struct{}{c.Implementer.ID: {}}
	vendors := make(map[string]struct{}, len(c.Reviewers))
	models := make(map[string]struct{}, len(c.Reviewers))
	implementerModelKey := strings.ToLower(c.Implementer.BaseURL + "\x00" + c.Implementer.Model)
	sharedWithImplementer := 0
	for _, reviewer := range c.Reviewers {
		if err := reviewer.validate(true); err != nil {
			return fmt.Errorf("reviewer: %w", err)
		}
		if _, exists := ids[reviewer.ID]; exists {
			return errors.New("reviewer ids contain duplicates")
		}
		ids[reviewer.ID] = struct{}{}
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
	if len(vendors) < 2 {
		return errors.New("reviewers must use at least two vendors")
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
	if strings.EqualFold(c.Readiness.Assessor.Vendor, c.Readiness.Checker.Vendor) {
		return errors.New("readiness assessor and checker must use different vendors")
	}
	if c.VendorHosts != nil {
		if err := validateVendorHosts(c.VendorHosts); err != nil {
			return err
		}
		endpoints := append([]ModelEndpoint{c.Implementer, c.Readiness.Assessor, c.Readiness.Checker}, c.Reviewers...)
		for _, endpoint := range endpoints {
			if err := vendorHostMatch(c.VendorHosts, endpoint); err != nil {
				return fmt.Errorf("%s: %w", endpoint.ID, err)
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

func (m ModelEndpoint) validate(reviewer bool) error {
	if !identifierPattern.MatchString(m.ID) || m.Vendor == "" || m.Model == "" ||
		strings.TrimSpace(m.Vendor) != m.Vendor || strings.TrimSpace(m.Model) != m.Model ||
		strings.ContainsAny(m.Vendor+m.Model, "\r\n\x00") || m.MaxOutputTokens < 128 || m.MaxOutputTokens > 32768 {
		return errors.New("model endpoint is invalid")
	}
	if err := validateModelBaseURL(m.BaseURL); err != nil {
		return err
	}
	if !apiKeyEnvPattern.MatchString(m.APIKeyEnv) {
		return errors.New("model api key environment name is invalid")
	}
	if reviewer && (m.Lens == "" || strings.TrimSpace(m.Lens) != m.Lens || len(m.Lens) > 512 || strings.ContainsAny(m.Lens, "\r\n\x00")) {
		return errors.New("reviewer lens is invalid")
	}
	if !reviewer && m.Lens != "" {
		return errors.New("implementer lens must be empty")
	}
	switch m.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return errors.New("model effort is invalid")
	}
	return nil
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
	return strings.HasSuffix(value, "/") && validRelativePath(strings.TrimSuffix(value, "/"))
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
	if parsed.Hostname() != strings.ToLower(parsed.Hostname()) {
		return errors.New("hostname must be lowercase")
	}
	return nil
}

func allowedPath(filename string, prefixes []string) bool {
	return slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(filename, prefix) })
}
