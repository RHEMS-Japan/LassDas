package attendant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// A delivery that starts on an exhausted key burns its whole allowance on
// refusals and ends as an unexplained model failure (live, 2026-09-02: the
// reception and the implementer shared one key, it ran dry in the
// afternoon, and every ticket after that died without a word about money).
// The attendant now asks the gateway first: one-token calls under every
// role's key, right before the reception starts. A refusal for budget holds
// the run — visibly, once, with automatic resumption — instead of spending
// it. Money is the only thing this gate decides: a rate limit, an outage or
// a transport error lets the run proceed and fail or succeed on its own.

const (
	budgetHoldFile      = "budget-hold.json"
	budgetRetryInterval = 10 * time.Minute
	budgetProbeTimeout  = 20 * time.Second
	budgetProbeMaxBody  = 4096
)

var budgetProbeClient = &http.Client{Timeout: budgetProbeTimeout}

type modelProbe struct {
	Role    string
	BaseURL string
	Model   string
	KeyEnv  string
}

type budgetHold struct {
	Roles []string  `json:"roles"`
	At    time.Time `json:"at"`
}

// roleProbes lists every key a delivery will use, each with one model it
// is allowed to call: the reception's direct calls from the consumer
// configuration and the three agent roles from the pod environment. Budget
// is a property of the key, so roles sharing a key share one probe and
// their names are joined.
func roleProbes(models worker.ModelConfig, getenv func(string) string) []modelProbe {
	var probes []modelProbe
	add := func(role, base, model, keyEnv string) {
		base = strings.TrimRight(base, "/")
		if base == "" || model == "" || keyEnv == "" {
			return
		}
		for index := range probes {
			if probes[index].BaseURL == base && probes[index].KeyEnv == keyEnv {
				probes[index].Role += " / " + role
				return
			}
		}
		probes = append(probes, modelProbe{Role: role, BaseURL: base, Model: model, KeyEnv: keyEnv})
	}
	add("受付 (対象の導出)", models.Implementer.BaseURL, models.Implementer.Model, models.Implementer.APIKeyEnv)
	add("受付 (検査 1 段目)", models.Readiness.Assessor.BaseURL, models.Readiness.Assessor.Model, models.Readiness.Assessor.APIKeyEnv)
	add("受付 (検査 2 段目)", models.Readiness.Checker.BaseURL, models.Readiness.Checker.Model, models.Readiness.Checker.APIKeyEnv)
	for _, reviewer := range models.Reviewers {
		add("レビュー役 ("+reviewer.ID+")", reviewer.BaseURL, reviewer.Model, reviewer.APIKeyEnv)
	}
	gateway := getenv("LASSDAS_GATEWAY_BASE_URL")
	add("実装役", gateway, getenv("LASSDAS_IMPLEMENTER_MODEL"), "LASSDAS_IMPLEMENTER_KEY")
	add("レビュー役 A", gateway, getenv("LASSDAS_REVIEW_A_MODEL"), "LASSDAS_REVIEW_A_KEY")
	add("レビュー役 B", gateway, getenv("LASSDAS_REVIEW_B_MODEL"), "LASSDAS_REVIEW_B_KEY")
	return probes
}

// probeBudget asks the gateway for one token under the role's key. Only a
// refusal about money counts as exhausted: the key's own budget cap, or
// the account's credit balance (the gateway answers both with 429 and
// `insufficient_quota`; a rate limit is a 429 without it).
func probeBudget(ctx context.Context, client *http.Client, probe modelProbe, key string) (exhausted bool, detail string, err error) {
	body, err := json.Marshal(map[string]any{
		"model": probe.Model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "ok"}},
	})
	if err != nil {
		return false, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(probe.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, "", err
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, budgetProbeMaxBody))
	if response.StatusCode == http.StatusTooManyRequests && refusalIsAboutMoney(string(raw)) {
		return true, strings.TrimSpace(string(raw)), nil
	}
	if response.StatusCode >= 400 {
		return false, fmt.Sprintf("http %d", response.StatusCode), nil
	}
	return false, "", nil
}

// refusalIsAboutMoney recognises the gateway's two money refusals by their
// wording — "budget exceeded" for a key's cap, "credit exhausted" /
// insufficient_quota for the account balance — and nothing else.
func refusalIsAboutMoney(body string) bool {
	text := strings.ToLower(body)
	return strings.Contains(text, "budget") || strings.Contains(text, "insufficient_quota") ||
		strings.Contains(text, "credit_exhausted") || strings.Contains(text, "credit exhausted")
}

func readBudgetHold(runDir string) (budgetHold, bool) {
	raw, err := os.ReadFile(filepath.Join(runDir, budgetHoldFile))
	if err != nil || len(raw) > 1<<16 {
		return budgetHold{}, false
	}
	var hold budgetHold
	if json.Unmarshal(raw, &hold) != nil || hold.At.IsZero() {
		return budgetHold{}, false
	}
	return hold, true
}

// budgetHeldRecently throttles the retry: a held run stays queued and
// unclaimed until the interval since the last refusal has passed.
func budgetHeldRecently(runDir string, now time.Time) bool {
	hold, held := readBudgetHold(runDir)
	return held && now.Before(hold.At.Add(budgetRetryInterval))
}

// loadModelConfig reads only the endpoints out of the consumer
// configuration. A lenient decode on purpose: the file's other sections
// are the worker's business and validated there; the probe needs
// addresses, not a contract.
func loadModelConfig(path string) (worker.ModelConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return worker.ModelConfig{}, err
	}
	if int64(len(raw)) > worker.MaxConfigJSONBytes {
		return worker.ModelConfig{}, fmt.Errorf("consumer config exceeds %d bytes", worker.MaxConfigJSONBytes)
	}
	var config struct {
		Models worker.ModelConfig `json:"models"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return worker.ModelConfig{}, err
	}
	return config.Models, nil
}

// checkBudgets probes every role and records a hold when any key is out of
// budget: the hold file (which throttles the retry and drives the board)
// and one ticket comment (marker-idempotent). A passing round clears an
// earlier hold. Returns whether the run is held.
func checkBudgets(ctx context.Context, config runtime.Config, backlog operatorConfirmationSource, run state.RunOverview, runDir string, issueID int64, getenv func(string) string, client *http.Client, logger Logger) bool {
	models, err := loadModelConfig(config.ConsumerConfigPath)
	if err != nil {
		logger.Error("budget check: consumer config unreadable; proceeding", "run", run.RunID, "error", err.Error())
		return false
	}
	for _, role := range [][2]string{{"実装役", "LASSDAS_IMPLEMENTER_MODEL"}, {"レビュー役 A", "LASSDAS_REVIEW_A_MODEL"}, {"レビュー役 B", "LASSDAS_REVIEW_B_MODEL"}} {
		if getenv(role[1]) == "" {
			logger.Error("budget check: agent role has no model in the environment and is not probed", "run", run.RunID, "role", role[0], "env", role[1])
		}
	}
	var exhausted []string
	for _, probe := range roleProbes(models, getenv) {
		key := getenv(probe.KeyEnv)
		if key == "" {
			logger.Error("budget check: credential missing; proceeding for this role", "run", run.RunID, "role", probe.Role, "env", probe.KeyEnv)
			continue
		}
		out, detail, err := probeBudget(ctx, client, probe, key)
		if err != nil {
			logger.Error("budget check: probe failed; proceeding for this role", "run", run.RunID, "role", probe.Role, "error", err.Error())
			continue
		}
		if out {
			exhausted = append(exhausted, probe.Role)
			logger.Info("budget check: key out of budget", "run", run.RunID, "role", probe.Role, "detail", detail)
		}
	}
	holdPath := filepath.Join(runDir, budgetHoldFile)
	if len(exhausted) == 0 {
		if _, held := readBudgetHold(runDir); held {
			_ = os.Remove(holdPath)
			logger.Info("budget check: hold cleared, run proceeds", "run", run.RunID)
		}
		return false
	}
	encoded, err := json.Marshal(budgetHold{Roles: exhausted, At: time.Now().UTC()})
	if err == nil {
		err = os.WriteFile(holdPath, encoded, 0o644)
	}
	if err != nil {
		logger.Error("budget hold: record failed", "run", run.RunID, "error", err.Error())
	}
	comments, err := backlog.ListComments(ctx, issueID, 0)
	if err != nil {
		logger.Error("budget hold: comment listing failed", "run", run.RunID, "error", err.Error())
		return true
	}
	if _, posted := commentIDWithMarker(comments, hook.CommentMarker(string(hook.RunCommentBudgetHold), run.RunID)); !posted {
		if _, err := backlog.AddComment(ctx, issueID, hook.BudgetHoldContent(run.RunID, exhausted)); err != nil {
			logger.Error("budget hold: notice post failed", "run", run.RunID, "error", err.Error())
		} else {
			logger.Info("budget hold: notice posted", "run", run.RunID, "roles", strings.Join(exhausted, ","))
		}
	}
	return true
}

// placeBudgetHold names the hold on the board: off the rail at intake,
// waiting for an operator, resuming by itself.
func placeBudgetHold(status *RunStatus, hold budgetHold) {
	status.placeAt("attention", "intake", "予算不足で開始できません",
		"運用担当者が利用枠の上限を上げると自動で再開します: "+strings.Join(hold.Roles, "、"))
}
