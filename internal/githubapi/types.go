package githubapi

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	ownerPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	workflowPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}\.ya?ml$`)
)

// Config fixes a client to one repository. RepositoryID is the immutable
// identity check; Owner and Repository are retained to build API paths.
type Config struct {
	Owner            string
	Repository       string
	RepositoryID     int64
	Token            string
	Timeout          time.Duration
	MaxResponseBytes int64
}

func (c Config) validate() error {
	if !ownerPattern.MatchString(c.Owner) {
		return invariant("invalid_owner")
	}
	if !repositoryPattern.MatchString(c.Repository) {
		return invariant("invalid_repository")
	}
	if c.RepositoryID <= 0 {
		return invariant("invalid_repository_id")
	}
	if c.Token == "" || strings.ContainsAny(c.Token, "\r\n") {
		return invariant("invalid_token")
	}
	if c.Timeout <= 0 {
		return invariant("invalid_http_timeout")
	}
	if c.MaxResponseBytes <= 0 || c.MaxResponseBytes > 16*1024*1024 {
		return invariant("invalid_response_limit")
	}
	return nil
}

type Contract struct {
	IntegrationBranch   string
	ReleaseBranch       string
	DefaultBranch       string
	MergeSettings       MergeSettings
	FeatureWorkflows    []WorkflowContract
	StagingWorkflow     WorkflowContract
	ProductionWorkflows []WorkflowContract
}

func (c Contract) validate() error {
	if err := validateBranch(c.IntegrationBranch); err != nil {
		return fmt.Errorf("integration branch: %w", err)
	}
	if err := validateBranch(c.ReleaseBranch); err != nil {
		return fmt.Errorf("release branch: %w", err)
	}
	if c.IntegrationBranch == c.ReleaseBranch {
		return invariant("release_branches_not_distinct")
	}
	// The default branch is pinned to one of the two delivery branches; which
	// one is the consumer's own convention and is verified against the live
	// repository, not legislated here.
	if c.DefaultBranch != c.ReleaseBranch && c.DefaultBranch != c.IntegrationBranch {
		return invariant("unexpected_default_branch_contract")
	}
	// Automated merges are merge commits, so that method must be enabled.
	// Whether people may additionally squash or rebase their own work is the
	// consumer's setting; the exact observed values are verified live.
	if !c.MergeSettings.AllowMergeCommit {
		return invariant("unsupported_merge_settings_contract")
	}
	if c.MergeSettings.MergeCommitTitle == "" || c.MergeSettings.MergeCommitMessage == "" || c.MergeSettings.SquashMergeCommitTitle == "" || c.MergeSettings.SquashMergeCommitMessage == "" {
		return invariant("incomplete_merge_settings_contract")
	}
	if len(c.FeatureWorkflows) > 4 || len(c.ProductionWorkflows) < 1 || len(c.ProductionWorkflows) > 4 {
		return invariant("unexpected_workflow_contract_count")
	}
	all := make([]WorkflowContract, 0, 5)
	all = append(all, c.FeatureWorkflows...)
	all = append(all, c.StagingWorkflow)
	all = append(all, c.ProductionWorkflows...)
	seenIDs := make(map[int64]struct{}, len(all))
	seenPaths := make(map[string]struct{}, len(all))
	for index, workflow := range all {
		requireJobs := index < len(c.FeatureWorkflows)
		if err := workflow.validate(requireJobs); err != nil {
			return err
		}
		if _, exists := seenIDs[workflow.ID]; exists {
			return invariant("duplicate_workflow_id")
		}
		if _, exists := seenPaths[workflow.Path]; exists {
			return invariant("duplicate_workflow_path")
		}
		seenIDs[workflow.ID] = struct{}{}
		seenPaths[workflow.Path] = struct{}{}
	}
	return nil
}

type MergeSettings struct {
	AllowMergeCommit          bool
	AllowSquashMerge          bool
	AllowRebaseMerge          bool
	AllowAutoMerge            bool
	AllowUpdateBranch         bool
	DeleteBranchOnMerge       bool
	UseSquashPRTitleAsDefault bool
	SquashMergeCommitTitle    string
	SquashMergeCommitMessage  string
	MergeCommitTitle          string
	MergeCommitMessage        string
	WebCommitSignoffRequired  bool
}

type WorkflowContract struct {
	ID           int64
	Name         string
	Path         string
	State        string
	RequiredJobs []string
}

func (w WorkflowContract) validate(requireJobs bool) error {
	if w.ID <= 0 || w.State != "active" || !strings.HasPrefix(w.Path, ".github/workflows/") {
		return invariant("invalid_workflow_contract")
	}
	filename := strings.TrimPrefix(w.Path, ".github/workflows/")
	if !workflowPattern.MatchString(filename) || strings.Contains(w.Path, "/../") {
		return invariant("invalid_workflow_contract")
	}
	if err := validateText(w.Name, 256, "invalid_workflow_contract"); err != nil {
		return err
	}
	if requireJobs && len(w.RequiredJobs) == 0 {
		return invariant("missing_required_workflow_jobs")
	}
	if !requireJobs && len(w.RequiredJobs) != 0 {
		return invariant("unexpected_deployment_jobs_contract")
	}
	seen := make(map[string]struct{}, len(w.RequiredJobs))
	for _, job := range w.RequiredJobs {
		if err := validateText(job, 256, "invalid_workflow_job"); err != nil {
			return err
		}
		if _, exists := seen[job]; exists {
			return invariant("duplicate_workflow_job")
		}
		seen[job] = struct{}{}
	}
	return nil
}

type Repository struct {
	ID                        int64
	FullName                  string
	DefaultBranch             string
	Archived                  bool
	Disabled                  bool
	AllowMergeCommit          bool
	AllowSquashMerge          bool
	AllowRebaseMerge          bool
	AllowAutoMerge            bool
	AllowUpdateBranch         bool
	DeleteBranchOnMerge       bool
	UseSquashPRTitleAsDefault bool
	SquashMergeCommitTitle    string
	SquashMergeCommitMessage  string
	MergeCommitTitle          string
	MergeCommitMessage        string
	WebCommitSignoffRequired  bool
}

type Workflow struct {
	ID    int64
	Name  string
	Path  string
	State string
}

type VerifiedRepository struct {
	Repository          Repository
	FeatureWorkflows    []Workflow
	StagingWorkflow     Workflow
	ProductionWorkflows []Workflow
}

type Snapshot struct {
	Branch  string
	SHA     string
	TreeSHA string
}

type Baseline struct {
	Integration      Snapshot
	Release          Snapshot
	MergeBaseSHA     string
	MergeBaseTreeSHA string
}

type FileUpdate struct {
	Path            string
	Content         []byte
	ExpectedBlobSHA string
}

type FeatureSpec struct {
	Branch              string
	CommitMessage       string
	AllowedPathPrefixes []string
	Files               []FileUpdate
}

type PublishedFeature struct {
	Base    Snapshot
	Branch  string
	HeadSHA string
	TreeSHA string
	Paths   []string
}

type PullRequest struct {
	Number       int64
	HTMLURL      string
	Title        string
	Body         string
	CreatedAt    time.Time
	HeadRef      string
	HeadSHA      string
	BaseRef      string
	BaseSHA      string
	HeadFullName string
}

type PullRequestSpec struct {
	Title string
	Body  string
}

type RequiredCheckRun struct {
	Name    string
	AppSlug string
}

type CheckRequirements struct {
	CheckRuns []RequiredCheckRun
	Statuses  []string
}

type CheckEvidence struct {
	PullRequestNumber int64
	HeadSHA           string
	WorkflowRunIDs    []int64
	WorkflowJobIDs    []int64
	CheckRunIDs       []int64
	StatusIDs         []int64
}

type WaitOptions struct {
	PollInterval time.Duration
	Timeout      time.Duration
}

func (o WaitOptions) validate() error {
	if o.PollInterval <= 0 || o.Timeout <= 0 || o.PollInterval > o.Timeout {
		return invariant("invalid_wait_options")
	}
	return nil
}

type MergeSpec struct {
	CommitTitle   string
	CommitMessage string
}

type MergeResult struct {
	PullRequestNumber int64
	BaseBranch        string
	BaseSHA           string
	HeadBranch        string
	HeadSHA           string
	MergeSHA          string
	TreeSHA           string
}

// MergeReflection is the narrow fact that GitHub accepted a merge, before the
// resulting commit, tree, branch ref, and deployment have been fully verified.
type MergeReflection struct {
	PullRequestNumber int64
	BaseBranch        string
	BaseSHA           string
	HeadBranch        string
	HeadSHA           string
	MergeSHA          string
}

type MergeReflectionRecorder func(MergeReflection) error

type WorkflowRun struct {
	ID                     int64
	WorkflowID             int64
	Name                   string
	DisplayTitle           string
	HTMLURL                string
	HeadBranch             string
	HeadSHA                string
	Event                  string
	Status                 string
	Conclusion             string
	Path                   string
	Attempt                int
	CreatedAt              time.Time
	UpdatedAt              time.Time
	RepositoryFullName     string
	HeadRepositoryFullName string
}

// DigestCommitPolicy describes the existing workflow-owned digest commit.
// ExactPaths is a set, not a prefix list: any unrelated file stops promotion.
type DigestCommitPolicy struct {
	Required           bool
	RequireDigestOnly  bool
	ExactMessagePrefix string
	ExactPaths         []string
	ActorLogin         string
}

type DeploymentResult struct {
	Merge           MergeResult
	WorkflowRuns    []WorkflowRun
	BranchHeadSHA   string
	DigestCommitSHA string
	DigestPaths     []string
}

type PromotionProof struct {
	Baseline                 Baseline
	Staging                  DeploymentResult
	ProductPaths             []string
	AcceptanceEvidenceSHA256 string
}

type InvariantError struct {
	Code string
}

func (e *InvariantError) Error() string { return "github invariant failed: " + e.Code }

func invariant(code string) error { return &InvariantError{Code: code} }

func IsInvariant(err error, code string) bool {
	var target *InvariantError
	return errors.As(err, &target) && target.Code == code
}

type APIError struct {
	Status int
	Code   string
	// Method and Endpoint name the API call that failed. They are the
	// difference between a diagnosable failure and a dead end, and they
	// never carry a credential - only a fixed path under /repos/.
	Method   string
	Endpoint string
}

func (e *APIError) Error() string {
	if e.Endpoint != "" {
		return fmt.Sprintf("github api failed: %s (status %d) at %s %s", e.Code, e.Status, e.Method, e.Endpoint)
	}
	return fmt.Sprintf("github api failed: %s (status %d)", e.Code, e.Status)
}

func isStatus(err error, status int) bool {
	var target *APIError
	return errors.As(err, &target) && target.Status == status
}

func isAmbiguousMutationError(err error) bool {
	var target *APIError
	if !errors.As(err, &target) {
		return false
	}
	switch target.Code {
	case "request_failed":
		return target.Status == 0
	case "response_read_failed", "response_too_large", "invalid_response":
		return target.Status >= http.StatusOK && target.Status < http.StatusMultipleChoices
	case "server_error":
		return target.Status >= http.StatusInternalServerError
	default:
		return false
	}
}

func isExistingResourceMutationError(err error) bool {
	var target *APIError
	return errors.As(err, &target) && (target.Status == http.StatusConflict || target.Status == http.StatusUnprocessableEntity)
}

func isTransientReadError(err error) bool {
	var target *APIError
	if !errors.As(err, &target) {
		return false
	}
	switch target.Code {
	case "request_failed", "response_read_failed", "response_too_large", "invalid_response", "server_error", "rate_limited":
		return true
	default:
		return false
	}
}

func validateBranch(branch string) error {
	if !branchPattern.MatchString(branch) || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") {
		return invariant("invalid_branch")
	}
	return nil
}

func validateText(value string, maximum int, code string) error {
	if value == "" || len(value) > maximum || strings.ContainsAny(value, "\x00\r") {
		return invariant(code)
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func newHTTPClient(config Config, transport http.RoundTripper) *http.Client {
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &http.Client{
		Transport: transport,
		Timeout:   config.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Validate exposes the contract invariants to configuration loading, which
// now owns these values (they were engine constants once, which is what kept
// a customer's name inside the engine binary).
func (c Contract) Validate() error {
	return c.validate()
}
