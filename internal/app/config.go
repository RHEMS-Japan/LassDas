package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

const runMarker = "Automation-Run-ID"

type RuntimeConfig struct {
	Hook        hook.Config
	FunctionURL hook.FunctionURLConfig
	Backlog     backlog.Config
	TableName   string
	// Dispatch is nil when instant wake-up is not configured; the worker then
	// starts on its schedule only.
	Dispatch *DispatchConfig
	// Board is nil when the board projection is not configured; tickets then
	// keep their statuses and the run is visible through comments only.
	Board *backlog.BoardStatusMap
}

type RuntimeSecrets struct {
	HookBasicUsername string `json:"hook_basic_username"`
	HookBasicPassword string `json:"hook_basic_password"`
	BacklogAPIKey     string `json:"backlog_api_key"`
	PullHMACKey       string `json:"pull_hmac_key"`
	// GitHubDispatchPrivateKeyB64 is optional: absent means the worker wakes
	// only on its schedule. When present it is the base64 of the dispatch
	// App's PEM key - an App that can start workflows in the instance
	// repository and nothing else.
	GitHubDispatchPrivateKeyB64 string `json:"github_dispatch_private_key_b64,omitempty"`
}

type SecretsManagerAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func LoadRuntimeSecrets(ctx context.Context, secretID string, api SecretsManagerAPI) (RuntimeSecrets, error) {
	if strings.TrimSpace(secretID) == "" || strings.TrimSpace(secretID) != secretID || strings.ContainsAny(secretID, "\r\n") || api == nil {
		return RuntimeSecrets{}, errors.New("runtime secret configuration is invalid")
	}
	output, err := api.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretID})
	if err != nil || output == nil || output.SecretString == nil || len(*output.SecretString) > 64*1024 {
		return RuntimeSecrets{}, errors.New("runtime secret could not be loaded")
	}
	decoder := json.NewDecoder(strings.NewReader(*output.SecretString))
	decoder.DisallowUnknownFields()
	var secrets RuntimeSecrets
	if err := decoder.Decode(&secrets); err != nil {
		return RuntimeSecrets{}, errors.New("runtime secret is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return RuntimeSecrets{}, errors.New("runtime secret is invalid")
	}
	if err := secrets.validate(); err != nil {
		return RuntimeSecrets{}, errors.New("runtime secret is invalid")
	}
	return secrets, nil
}

func (s RuntimeSecrets) validate() error {
	values := []string{s.HookBasicUsername, s.HookBasicPassword, s.BacklogAPIKey, s.PullHMACKey}
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return errors.New("secret value is invalid")
		}
	}
	if strings.Contains(s.HookBasicUsername, ":") {
		return errors.New("basic username is invalid")
	}
	if _, err := hook.DecodePullHMACKey(s.PullHMACKey); err != nil {
		return errors.New("pull key is invalid")
	}
	if s.GitHubDispatchPrivateKeyB64 != "" &&
		(strings.TrimSpace(s.GitHubDispatchPrivateKeyB64) != s.GitHubDispatchPrivateKeyB64 || strings.ContainsAny(s.GitHubDispatchPrivateKeyB64, "\r\n")) {
		return errors.New("dispatch key is invalid")
	}
	return nil
}

func LoadConfig(getenv func(string) string, secrets RuntimeSecrets) (RuntimeConfig, error) {
	if getenv == nil {
		return RuntimeConfig{}, errors.New("environment reader is required")
	}
	if err := secrets.validate(); err != nil {
		return RuntimeConfig{}, errors.New("runtime secrets are invalid")
	}
	projectID, err := positiveInt64(getenv("BACKLOG_PROJECT_ID"))
	if err != nil {
		return RuntimeConfig{}, errors.New("backlog project id is invalid")
	}
	creatorID, err := positiveInt64(getenv("BACKLOG_ALLOWED_CREATOR_ID"))
	if err != nil {
		return RuntimeConfig{}, errors.New("backlog creator id is invalid")
	}
	repositoryID, err := positiveInt64(getenv("PULL_REPOSITORY_ID"))
	if err != nil {
		return RuntimeConfig{}, errors.New("pull repository id is invalid")
	}
	// The category gate is opt-in for the deployment too: an absent value keeps
	// the pre-gate behaviour of queueing every issue the creator files.
	requiredCategoryID := int64(0)
	if raw := getenv("TICKET_INGRESS_REQUIRED_CATEGORY_ID"); raw != "" {
		requiredCategoryID, err = positiveInt64(raw)
		if err != nil {
			return RuntimeConfig{}, errors.New("required category id is invalid")
		}
	}
	// Instant wake-up is all-or-nothing: a key without a target, or a target
	// without a key, is a half-configured deployment and refuses to start.
	var dispatch *DispatchConfig
	dispatchEnv := DispatchConfig{
		Repository: getenv("DISPATCH_REPOSITORY"), Workflow: getenv("DISPATCH_WORKFLOW"),
		Ref: getenv("DISPATCH_REF"), PrivateKeyB64: secrets.GitHubDispatchPrivateKeyB64,
	}
	dispatchAppID := getenv("DISPATCH_APP_ID")
	dispatchInstallationID := getenv("DISPATCH_INSTALLATION_ID")
	if dispatchEnv.Repository != "" || dispatchEnv.Workflow != "" || dispatchEnv.Ref != "" ||
		dispatchEnv.PrivateKeyB64 != "" || dispatchAppID != "" || dispatchInstallationID != "" {
		if dispatchEnv.AppID, err = positiveInt64(dispatchAppID); err != nil {
			return RuntimeConfig{}, errors.New("dispatch app id is invalid")
		}
		if dispatchEnv.InstallationID, err = positiveInt64(dispatchInstallationID); err != nil {
			return RuntimeConfig{}, errors.New("dispatch installation id is invalid")
		}
		if err := dispatchEnv.validate(); err != nil {
			return RuntimeConfig{}, err
		}
		dispatch = &dispatchEnv
	}
	// The board projection is all-or-nothing for the same reason as instant
	// wake-up: a half-mapped board would show some phases and silently drop
	// others, which reads as state that never happened.
	var board *backlog.BoardStatusMap
	boardRunning := getenv("BOARD_STATUS_RUNNING")
	boardAwaiting := getenv("BOARD_STATUS_AWAITING_ANSWER")
	boardDelivered := getenv("BOARD_STATUS_DELIVERED")
	boardAttention := getenv("BOARD_STATUS_NEEDS_ATTENTION")
	if boardRunning != "" || boardAwaiting != "" || boardDelivered != "" || boardAttention != "" {
		statuses := backlog.BoardStatusMap{}
		if statuses.Running, err = positiveInt64(boardRunning); err != nil {
			return RuntimeConfig{}, errors.New("board status map is invalid")
		}
		if statuses.AwaitingAnswer, err = positiveInt64(boardAwaiting); err != nil {
			return RuntimeConfig{}, errors.New("board status map is invalid")
		}
		if statuses.Delivered, err = positiveInt64(boardDelivered); err != nil {
			return RuntimeConfig{}, errors.New("board status map is invalid")
		}
		if statuses.NeedsAttention, err = positiveInt64(boardAttention); err != nil {
			return RuntimeConfig{}, errors.New("board status map is invalid")
		}
		board = &statuses
	}
	pullKey, err := hook.DecodePullHMACKey(secrets.PullHMACKey)
	if err != nil {
		return RuntimeConfig{}, errors.New("runtime secrets are invalid")
	}
	// Where the automation may deliver is configuration, not code: one JSON
	// value naming each destination, its stopping point, and the origins its
	// screenshots may come from.
	destinations, err := decodeReportDestinations(getenv("REPORT_DESTINATIONS"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	target := hook.DeliveryTarget{RepositoryID: repositoryID, WorkflowRefSHA256: getenv("PULL_WORKFLOW_REF_SHA256")}
	pull := hook.PullRouteConfig{
		HMACKey: pullKey, RepositoryID: repositoryID, RepositorySHA256: getenv("PULL_REPOSITORY_SHA256"),
		WorkflowRefSHA256: getenv("PULL_WORKFLOW_REF_SHA256"),
		SpaceKey:          getenv("BACKLOG_SPACE_KEY"), ProjectID: projectID, ProjectKey: getenv("BACKLOG_PROJECT_KEY"),
		AllowedCreatorID: creatorID, AllowedActivityType: 1,
		Target: target, ClockSkew: 2 * time.Minute,
	}
	config := RuntimeConfig{
		Hook: hook.Config{
			SpaceKey: getenv("BACKLOG_SPACE_KEY"), ProjectID: projectID, ProjectKey: getenv("BACKLOG_PROJECT_KEY"),
			AllowedCreatorID: creatorID, AllowedActivityType: 1,
			RequiredCategoryID: requiredCategoryID,
			RunMarker:          runMarker, ExpectedRunID: getenv("AUTOMATION_RUN_ID"), Target: target,
			MaxEnvelopeBytes: 60 * 1024,
		},
		FunctionURL: hook.FunctionURLConfig{
			BasicUsername: secrets.HookBasicUsername, BasicPassword: secrets.HookBasicPassword, MaxBodyBytes: 64 * 1024,
			Pull: pull,
			Report: hook.ReportRouteConfig{
				HMACKey: pullKey, RepositoryID: repositoryID, RepositorySHA256: getenv("PULL_REPOSITORY_SHA256"),
				WorkflowRefSHA256: getenv("PULL_WORKFLOW_REF_SHA256"), ExpectedRunID: getenv("AUTOMATION_RUN_ID"),
				Destinations: destinations,
				ClockSkew:    2 * time.Minute, LeaseDuration: 2 * time.Minute,
				SpaceKey: getenv("BACKLOG_SPACE_KEY"), ProjectID: projectID, ProjectKey: getenv("BACKLOG_PROJECT_KEY"),
				AllowedCreatorID: creatorID, AllowedActivityType: 1, Target: target,
			},
		},
		Dispatch: dispatch,
		Board:    board,
		Backlog: backlog.Config{
			SpaceKey: getenv("BACKLOG_SPACE_KEY"), Origin: getenv("BACKLOG_ORIGIN"), APIKey: secrets.BacklogAPIKey,
			Timeout: 10 * time.Second, MaxResponseBytes: 1024 * 1024,
		},
		TableName: getenv("QUEUE_TABLE_NAME"),
	}
	if config.TableName == "" {
		return RuntimeConfig{}, errors.New("queue table is empty")
	}
	if err := config.Hook.Validate(); err != nil {
		return RuntimeConfig{}, errors.New("hook configuration is invalid")
	}
	if err := config.FunctionURL.Validate(); err != nil {
		return RuntimeConfig{}, errors.New("function url configuration is invalid")
	}
	if err := config.Backlog.Validate(); err != nil {
		return RuntimeConfig{}, errors.New("backlog configuration is invalid")
	}
	return config, nil
}

// decodeReportDestinations reads the delivery destinations the hook accepts
// reports for. An absent value leaves the report route unconfigured, which
// disables it rather than accepting anything.
func decodeReportDestinations(value string) ([]hook.ReportDestination, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var destinations []hook.ReportDestination
	if err := decoder.Decode(&destinations); err != nil {
		return nil, errors.New("report destinations are invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("report destinations are invalid")
	}
	if len(destinations) == 0 {
		return nil, errors.New("report destinations are invalid")
	}
	return destinations, nil
}

func positiveInt64(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, errors.New("value must be a positive integer")
	}
	return parsed, nil
}
