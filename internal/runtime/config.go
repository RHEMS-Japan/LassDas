// Package runtime holds the configuration of the single-pod constitution:
// the engine running as a Hermes kanban worker, with the ledger in a local
// SQLite file and the attendant polling the tracker in the same pod (docs:
// HERMES_AS_LASSDAS_RUNTIME). It is the local sibling of the Lambda-side
// internal/app configuration — the same facts, read from one JSON file plus
// the secrets that stay in the environment.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"automation.internal/ticket-ingress/internal/hook"
)

// Config is the runtime.json the operator mounts next to the engine. It
// carries no secrets — those arrive in the environment (BACKLOG_API_KEY,
// model keys) exactly as the worker's agent contract already expects.
type Config struct {
	// LedgerPath is the SQLite ledger file. The attendant and the runner
	// must point at the same file; the single-writer transaction is what
	// serializes them.
	LedgerPath string `json:"ledger_path"`

	// Tracker is entrance A's first adapter (Backlog).
	Tracker TrackerConfig `json:"tracker"`

	// Identity pins who the runner is in the ledger's terms. The ledger
	// seals an owner into every claim; under GitHub Actions that was the
	// workflow run. Here it is this pod's engine: fixed identity digests
	// plus the per-run Hermes task run id.
	Identity IdentityConfig `json:"identity"`

	// AutomationRunID is the deployment's fixed run marker (the Lambda's
	// AUTOMATION_RUN_ID): the routeful-but-recordless identity ticks and
	// notices authenticate as, rebound to the live run via the pending
	// slot. Format: run_YYYYMMDD_<24 hex>.
	AutomationRunID string `json:"automation_run_id"`

	// ReportDestinations names where the automation may deliver, each with
	// its stopping point and evidence origins — the Lambda's
	// REPORT_DESTINATIONS JSON, verbatim.
	ReportDestinations []hook.ReportDestination `json:"report_destinations"`

	// ConsumerConfigPath is the existing m1-consumer.json (models, agents,
	// consumers). The stage runner reads it exactly as the workflow did.
	ConsumerConfigPath string `json:"consumer_config_path"`

	// KnowledgeRoot is the instance knowledge directory handed to the
	// model stages (the workflow's --knowledge-root).
	KnowledgeRoot string `json:"knowledge_root"`

	// WorkerBin is the path of the stage-step binary (cmd/worker). The
	// runner shells out to it exactly as the workflow did.
	WorkerBin string `json:"worker_bin"`
	// ControllerBin is the delivery binary (cmd/controller).
	ControllerBin string `json:"controller_bin"`
	// BrowserCheckBin is the visible-evidence binary (cmd/browsercheck).
	BrowserCheckBin string `json:"browsercheck_bin"`

	// HermesBin is the hermes CLI used for the one canonical card
	// transition the runner itself makes (blocking its own card while a
	// question waits) and by the attendant for card creation/unblock.
	HermesBin string `json:"hermes_bin"`
	// HermesBoard is the kanban board slug the cards live on.
	HermesBoard string `json:"hermes_board"`
	// HermesProfile is the assignee profile whose worker.command runs the
	// stage runner.
	HermesProfile string `json:"hermes_profile"`

	// WorkerSHA256/ControllerSHA256 optionally pin the stage binaries. The
	// workflow measured its tools before every use (checkout SHA + binary
	// digests); the pod cannot measure a git checkout, but when these pins
	// are set the runner refuses to start with binaries whose digests do
	// not match, so a sealed tool SHA cannot silently name other code.
	WorkerSHA256     string `json:"worker_sha256,omitempty"`
	ControllerSHA256 string `json:"controller_sha256,omitempty"`
}

type TrackerConfig struct {
	Origin             string `json:"origin"`
	SpaceKey           string `json:"space_key"`
	ProjectID          int64  `json:"project_id"`
	ProjectKey         string `json:"project_key"`
	AllowedCreatorID   int64  `json:"allowed_creator_id"`
	AllowedActivityType int   `json:"allowed_activity_type"`
	RequiredCategoryID int64  `json:"required_category_id"`
	BoardStatuses      BoardStatuses `json:"board_statuses"`
}

// IdentityConfig fixes the owner identity the ledger seals into claims.
// RepositoryID/digests were the GitHub workflow's identity; locally they
// are stable names for "this engine in this pod". EngineSHA is the build's
// commit (40 hex), stamped at build time or provided by the operator.
type IdentityConfig struct {
	RepositoryID int64  `json:"repository_id"`
	Repository   string `json:"repository"`
	WorkflowRef  string `json:"workflow_ref"`
	EngineSHA    string `json:"engine_sha"`
}

var (
	commit40 = regexp.MustCompile(`^[a-f0-9]{40}$`)
	// ownerNamePattern is the owner/name form the run-reference URL embeds;
	// any other value would pass config load and then have every report
	// refused at the route gate — a fail-late brick this check fails early.
	ownerNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	sha256Pattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// Load reads and validates the runtime configuration.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("runtime config unreadable: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("runtime config invalid: %w", err)
	}
	if config.LedgerPath == "" || config.ConsumerConfigPath == "" || config.KnowledgeRoot == "" ||
		config.WorkerBin == "" || config.ControllerBin == "" {
		return Config{}, errors.New("runtime config: ledger_path, consumer_config_path, knowledge_root, worker_bin and controller_bin are required")
	}
	t := config.Tracker
	if t.Origin == "" || t.SpaceKey == "" || t.ProjectID <= 0 || t.ProjectKey == "" ||
		t.AllowedCreatorID <= 0 {
		return Config{}, errors.New("runtime config: tracker origin, space_key, project_id, project_key and allowed_creator_id are required")
	}
	// The Lambda hardcoded activity type 1 (issue created) on every route;
	// keeping it fixed here preserves exactly which tracker activities can
	// become runs. Widening it is a protocol change, not a config knob.
	if t.AllowedActivityType != 1 {
		return Config{}, errors.New("runtime config: allowed_activity_type must be 1 (issue created)")
	}
	i := config.Identity
	if i.RepositoryID <= 0 || !ownerNamePattern.MatchString(i.Repository) ||
		i.WorkflowRef == "" || !commit40.MatchString(i.EngineSHA) {
		return Config{}, errors.New("runtime config: identity needs repository_id, repository as owner/name, workflow_ref and a 40-hex engine_sha")
	}
	for _, pin := range []string{config.WorkerSHA256, config.ControllerSHA256} {
		if pin != "" && !sha256Pattern.MatchString(pin) {
			return Config{}, errors.New("runtime config: binary sha256 pins must be 64 hex")
		}
	}
	if !hook.ValidRunID(config.AutomationRunID) {
		return Config{}, errors.New("runtime config: automation_run_id is required (run_YYYYMMDD_<24 hex>)")
	}
	if len(config.ReportDestinations) == 0 {
		return Config{}, errors.New("runtime config: report_destinations is required")
	}
	return config, nil
}

// BoardStatuses are the tracker's own status ids for the four phases the
// engine projects (the Lambda's BOARD_STATUS_* env, as one object).
type BoardStatuses struct {
	Running        int64 `json:"running"`
	AwaitingAnswer int64 `json:"awaiting_answer"`
	Delivered      int64 `json:"delivered"`
	NeedsAttention int64 `json:"needs_attention"`
}

// Target is the sealed delivery target this identity corresponds to — the
// same shape the receiver sealed from GitHub env, computed from the fixed
// local identity instead.
func (c Config) Target() hook.DeliveryTarget {
	return hook.DeliveryTarget{
		RepositoryID:      c.Identity.RepositoryID,
		WorkflowRefSHA256: hook.HashIdentity(c.Identity.WorkflowRef),
	}
}

// Owner returns the claim owner for one Hermes task run. hermesRunID is the
// numeric HERMES_KANBAN_RUN_ID of the dispatched worker; each re-dispatch
// gets a fresh run id, which is exactly the per-attempt identity the ledger
// wants.
func (c Config) Owner(hermesRunID int64) hook.PullOwner {
	return hook.PullOwner{
		RepositoryID:      c.Identity.RepositoryID,
		RepositorySHA256:  hook.HashIdentity(c.Identity.Repository),
		WorkflowRefSHA256: hook.HashIdentity(c.Identity.WorkflowRef),
		WorkflowSHA:       c.Identity.EngineSHA,
		WorkflowRunID:     hermesRunID,
		RunAttempt:        1,
	}
}
