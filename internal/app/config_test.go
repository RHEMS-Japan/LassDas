package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/hook"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

func validEnvironment() map[string]string {
	return map[string]string{
		"BACKLOG_SPACE_KEY":          "example-space",
		"BACKLOG_ORIGIN":             "https://example-space.backlog.com",
		"BACKLOG_PROJECT_ID":         "101",
		"BACKLOG_PROJECT_KEY":        "TICKET",
		"BACKLOG_ALLOWED_CREATOR_ID": "202",
		"AUTOMATION_RUN_ID":          "run_20260802_alpha",
		"PULL_REPOSITORY_ID":         "123456",
		"PULL_REPOSITORY_SHA256":     hook.HashIdentity("example/source"),
		"PULL_WORKFLOW_REF_SHA256":   strings.Repeat("b", 64),
		"REPORT_DESTINATIONS":        `[{"repository":"example/target","delivery":"production","staging_origin":"https://staging.example.com","production_origin":"https://www.example.com"}]`,
		"QUEUE_TABLE_NAME":           "ticket-ingress-queue",
	}
}

func validSecrets() RuntimeSecrets {
	return RuntimeSecrets{
		HookBasicUsername: "hook-user",
		HookBasicPassword: "hook-password",
		BacklogAPIKey:     "backlog-secret",
		PullHMACKey:       base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)),
	}
}

func load(t *testing.T, values map[string]string) (RuntimeConfig, error) {
	t.Helper()
	return LoadConfig(func(key string) string { return values[key] }, validSecrets())
}

func TestLoadConfigBindsOneFixedRoute(t *testing.T) {
	values := validEnvironment()
	config, err := load(t, values)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Hook.Target != config.FunctionURL.Pull.Target {
		t.Fatalf("hook target = %+v, pull target = %+v", config.Hook.Target, config.FunctionURL.Pull.Target)
	}
	if config.Hook.AllowedActivityType != 1 || config.Hook.RunMarker != "Automation-Run-ID" {
		t.Fatalf("activity type = %d, run marker = %q", config.Hook.AllowedActivityType, config.Hook.RunMarker)
	}
	if config.FunctionURL.Report.Target != config.Hook.Target || config.FunctionURL.Report.ExpectedRunID != config.Hook.ExpectedRunID {
		t.Fatalf("report route is not bound to the hook route: %+v", config.FunctionURL.Report)
	}
}

func TestLoadConfigFailsClosedForEverySecurityBoundary(t *testing.T) {
	keys := []string{
		"BACKLOG_SPACE_KEY", "BACKLOG_ORIGIN",
		"BACKLOG_PROJECT_ID", "BACKLOG_PROJECT_KEY", "BACKLOG_ALLOWED_CREATOR_ID", "AUTOMATION_RUN_ID",
		"PULL_REPOSITORY_ID", "PULL_REPOSITORY_SHA256", "PULL_WORKFLOW_REF_SHA256",
		"REPORT_DESTINATIONS",
		"QUEUE_TABLE_NAME",
	}
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			values := validEnvironment()
			values[key] = ""
			if _, err := load(t, values); err == nil {
				t.Fatal("LoadConfig() accepted an empty security setting")
			}
		})
	}
}

func TestLoadConfigRejectsUnsafeOriginAndNumericAllowlist(t *testing.T) {
	tests := map[string]func(map[string]string){
		"http origin":  func(v map[string]string) { v["BACKLOG_ORIGIN"] = "http://example-space.backlog.com" },
		"wrong host":   func(v map[string]string) { v["BACKLOG_ORIGIN"] = "https://attacker.invalid" },
		"project zero": func(v map[string]string) { v["BACKLOG_PROJECT_ID"] = "0" },
		"creator text": func(v map[string]string) { v["BACKLOG_ALLOWED_CREATOR_ID"] = "any" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			values := validEnvironment()
			mutate(values)
			if _, err := load(t, values); err == nil {
				t.Fatal("LoadConfig() accepted unsafe configuration")
			}
		})
	}
}

func TestLoadConfigErrorsNeverContainSecrets(t *testing.T) {
	const secret = "CONFIG-SECRET-SENTINEL"
	secrets := validSecrets()
	secrets.BacklogAPIKey = secret + "\n"
	_, err := LoadConfig(func(key string) string { return validEnvironment()[key] }, secrets)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %v", err)
	}
}

type fakeSecretsManager struct {
	value string
	err   error
}

func (f fakeSecretsManager) GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &secretsmanager.GetSecretValueOutput{SecretString: &f.value}, nil
}

func TestLoadRuntimeSecrets(t *testing.T) {
	value := `{"hook_basic_username":"hook-user","hook_basic_password":"hook-password","backlog_api_key":"backlog-secret","pull_hmac_key":"` + validSecrets().PullHMACKey + `"}`
	secrets, err := LoadRuntimeSecrets(context.Background(), "ticket-automation/runtime", fakeSecretsManager{value: value})
	if err != nil {
		t.Fatalf("LoadRuntimeSecrets() error = %v", err)
	}
	if secrets != validSecrets() {
		t.Fatalf("secrets did not match expected fields")
	}
}

func TestLoadRuntimeSecretsFailsClosedWithoutLeaking(t *testing.T) {
	const sentinel = "SECRET-MANAGER-SENTINEL"
	tests := map[string]fakeSecretsManager{
		"manager error": {err: errors.New(sentinel)},
		"unknown field": {value: `{"hook_basic_username":"u","hook_basic_password":"p","backlog_api_key":"b","pull_hmac_key":"` + validSecrets().PullHMACKey + `","extra":"` + sentinel + `"}`},
		"old token":     {value: `{"hook_basic_username":"u","hook_basic_password":"p","backlog_api_key":"b","pull_hmac_key":"` + validSecrets().PullHMACKey + `","github_token":"` + sentinel + `"}`},
		"newline":       {value: `{"hook_basic_username":"u","hook_basic_password":"p","backlog_api_key":"b\n` + sentinel + `","pull_hmac_key":"` + validSecrets().PullHMACKey + `"}`},
	}
	for name, api := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRuntimeSecrets(context.Background(), "ticket-automation/runtime", api)
			if err == nil || strings.Contains(err.Error(), sentinel) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadConfigReadsTheOptionalCategoryGate(t *testing.T) {
	values := validEnvironment()
	config, err := load(t, values)
	if err != nil || config.Hook.RequiredCategoryID != 0 {
		t.Fatalf("absent gate: id = %d, error = %v", config.Hook.RequiredCategoryID, err)
	}

	values["TICKET_INGRESS_REQUIRED_CATEGORY_ID"] = "4062933"
	config, err = load(t, values)
	if err != nil || config.Hook.RequiredCategoryID != 4062933 {
		t.Fatalf("set gate: id = %d, error = %v", config.Hook.RequiredCategoryID, err)
	}

	for _, invalid := range []string{"0", "-5", "abc"} {
		values["TICKET_INGRESS_REQUIRED_CATEGORY_ID"] = invalid
		if _, err := load(t, values); err == nil {
			t.Fatalf("LoadConfig() accepted category id %q", invalid)
		}
	}
}

func fullDispatchEnvironment() map[string]string {
	values := validEnvironment()
	values["DISPATCH_REPOSITORY"] = "example/instance"
	values["DISPATCH_WORKFLOW"] = "receive.yml"
	values["DISPATCH_REF"] = "main"
	values["DISPATCH_APP_ID"] = "4001"
	values["DISPATCH_INSTALLATION_ID"] = "9001"
	return values
}

func TestLoadConfigRequiresACompleteDispatchOrNone(t *testing.T) {
	base := validEnvironment()
	if config, err := load(t, base); err != nil || config.Dispatch != nil {
		t.Fatalf("no dispatch settings: dispatch = %+v, error = %v", config.Dispatch, err)
	}

	config, err := loadWithSecrets(t, fullDispatchEnvironment(), testDispatchKeyB64())
	if err != nil || config.Dispatch == nil || config.Dispatch.AppID != 4001 ||
		config.Dispatch.InstallationID != 9001 || config.Dispatch.Repository != "example/instance" {
		t.Fatalf("full dispatch settings: dispatch = %+v, error = %v", config.Dispatch, err)
	}

	partials := map[string]func() (RuntimeConfig, error){
		"key without target": func() (RuntimeConfig, error) {
			return loadWithSecrets(t, validEnvironment(), testDispatchKeyB64())
		},
		"target without key": func() (RuntimeConfig, error) {
			return load(t, fullDispatchEnvironment())
		},
		"repository only": func() (RuntimeConfig, error) {
			values := validEnvironment()
			values["DISPATCH_REPOSITORY"] = "example/instance"
			return load(t, values)
		},
		"missing installation id": func() (RuntimeConfig, error) {
			values := fullDispatchEnvironment()
			delete(values, "DISPATCH_INSTALLATION_ID")
			return loadWithSecrets(t, values, testDispatchKeyB64())
		},
	}
	for name, attempt := range partials {
		t.Run(name, func(t *testing.T) {
			if _, err := attempt(); err == nil {
				t.Fatal("a half-configured dispatch was accepted")
			}
		})
	}
}

func fullBoardEnvironment() map[string]string {
	values := validEnvironment()
	values["BOARD_STATUS_RUNNING"] = "439072"
	values["BOARD_STATUS_AWAITING_ANSWER"] = "439073"
	values["BOARD_STATUS_DELIVERED"] = "3"
	values["BOARD_STATUS_NEEDS_ATTENTION"] = "1"
	return values
}

// A half-mapped board would show some phases and silently drop others, which
// reads as state that never happened - the same all-or-nothing contract as
// instant wake-up, and the same test shape.
func TestLoadConfigRequiresACompleteBoardOrNone(t *testing.T) {
	if config, err := load(t, validEnvironment()); err != nil || config.Board != nil {
		t.Fatalf("no board settings: board = %+v, error = %v", config.Board, err)
	}

	config, err := load(t, fullBoardEnvironment())
	if err != nil || config.Board == nil || config.Board.Running != 439072 ||
		config.Board.AwaitingAnswer != 439073 || config.Board.Delivered != 3 || config.Board.NeedsAttention != 1 {
		t.Fatalf("full board settings: board = %+v, error = %v", config.Board, err)
	}

	for _, missing := range []string{
		"BOARD_STATUS_RUNNING", "BOARD_STATUS_AWAITING_ANSWER", "BOARD_STATUS_DELIVERED", "BOARD_STATUS_NEEDS_ATTENTION",
	} {
		t.Run("missing "+missing, func(t *testing.T) {
			values := fullBoardEnvironment()
			delete(values, missing)
			if _, err := load(t, values); err == nil {
				t.Fatal("a half-configured board was accepted")
			}
		})
	}
	t.Run("one value only", func(t *testing.T) {
		values := validEnvironment()
		values["BOARD_STATUS_RUNNING"] = "439072"
		if _, err := load(t, values); err == nil {
			t.Fatal("a single board value was accepted")
		}
	})
	t.Run("non-numeric value", func(t *testing.T) {
		values := fullBoardEnvironment()
		values["BOARD_STATUS_DELIVERED"] = "done"
		if _, err := load(t, values); err == nil {
			t.Fatal("a non-numeric board status was accepted")
		}
	})
}

func loadWithSecrets(t *testing.T, values map[string]string, dispatchKeyB64 string) (RuntimeConfig, error) {
	t.Helper()
	secrets := validSecrets()
	secrets.GitHubDispatchPrivateKeyB64 = dispatchKeyB64
	return LoadConfig(func(key string) string { return values[key] }, secrets)
}
