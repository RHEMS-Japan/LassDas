package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxSpendResponseBytes bounds the spend endpoint's reply. The document is a
// handful of numbers; anything larger is a wrong endpoint, not a big answer.
const MaxSpendResponseBytes = 4096

// KeySpend is what one key was billed for a window of time, as the gateway
// reports it: the amount that will appear on the invoice, not an estimate from
// token counts multiplied by a price table this process would have to keep in
// sync. Unpriced counts calls the gateway could not settle a price for - those
// are missing from SpendUSD, so a run that has any cannot claim a total.
type KeySpend struct {
	// KeyEnv is the environment variable naming the key. Roles are mapped back
	// to a key through it, so the key itself is never stored or logged.
	KeyEnv   string  `json:"key_env"`
	KeyName  string  `json:"key_name"`
	SpendUSD float64 `json:"spend_usd"`
	Unpriced int     `json:"unpriced_requests"`
	// AlsoKeyEnvs names further variables that resolved to this same key, so
	// the report can list every role the one figure covers.
	AlsoKeyEnvs []string `json:"also_key_envs,omitempty"`
	// Approximate is set when the figure is the difference between two
	// readings of the key's running total rather than a billing window.
	// Anything else billed to the same key while this run worked is inside
	// it, so the report says what kind of number it is showing.
	Approximate bool `json:"approximate,omitempty"`
}

// SpendReader reads what one key was billed since a point in time.
type SpendReader interface {
	SpendSince(ctx context.Context, baseURL, apiKeyEnv string, since time.Time) (KeySpend, error)
}

// GatewaySpendReader calls GET {baseURL}/key/spend?since=, which returns the
// calling key's own figure and nothing else. It never retries: a missing
// reading makes a run report "not available", which is honest, whereas
// retrying against a billing endpoint is not worth the risk of double counting.
type GatewaySpendReader struct {
	client *http.Client
}

func NewGatewaySpendReader(client *http.Client) (*GatewaySpendReader, error) {
	if client == nil {
		return nil, errors.New("HTTP client is required")
	}
	return &GatewaySpendReader{client: client}, nil
}

func (g *GatewaySpendReader) SpendSince(ctx context.Context, baseURL, apiKeyEnv string, since time.Time) (KeySpend, error) {
	if g == nil || g.client == nil || ctx == nil {
		return KeySpend{}, safeModelError("spend transport is invalid")
	}
	if baseURL == "" || since.IsZero() {
		return KeySpend{}, safeModelError("spend request is invalid")
	}
	apiKey := os.Getenv(apiKeyEnv)
	if apiKeyEnv == "" || apiKey == "" || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, "\r\n\x00") {
		return KeySpend{}, safeModelError("spend API key is unavailable")
	}
	endpoint := baseURL + "/key/spend?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return KeySpend{}, safeModelError("spend request could not be built")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := g.client.Do(request)
	if err != nil {
		return KeySpend{}, safeModelError("spend request failed: " + err.Error())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return KeySpend{}, safeModelError("spend endpoint returned " + response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxSpendResponseBytes+1))
	if err != nil || len(body) > MaxSpendResponseBytes {
		return KeySpend{}, safeModelError("spend response could not be read")
	}
	var parsed struct {
		KeyName  string   `json:"key_name"`
		SpendUSD *float64 `json:"spend_usd"`
		Unpriced int      `json:"unpriced_requests"`
	}
	// An absent number is not a zero: reporting zero here would print "this
	// ticket cost nothing" every time the endpoint misbehaves.
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.SpendUSD == nil || *parsed.SpendUSD < 0 || parsed.Unpriced < 0 {
		return KeySpend{}, safeModelError("spend response is invalid")
	}
	return KeySpend{KeyEnv: apiKeyEnv, KeyName: parsed.KeyName, SpendUSD: *parsed.SpendUSD, Unpriced: parsed.Unpriced}, nil
}

// RunSpend is what one ticket cost, per key and in total.
type RunSpend struct {
	// Complete is false when a key could not be read or the gateway left calls
	// unpriced. Total is then a floor, and the report says so rather than
	// printing a number that reads as the whole bill.
	Complete bool       `json:"complete"`
	TotalUSD float64    `json:"total_usd"`
	Keys     []KeySpend `json:"keys"`
	// Approximate is set when any figure is a difference of running totals.
	Approximate bool `json:"approximate,omitempty"`
}

// SpendKeyEnvs lists every distinct key the configured roles bill against, in a
// stable order. One key often serves two roles (an implementer key that also
// drafts readiness), which is why spend is keyed by environment variable rather
// than by role: the gateway bills the key, not the seat.
func SpendKeyEnvs(config Config) []string {
	seen := map[string]struct{}{}
	for _, endpoint := range spendEndpoints(config) {
		if endpoint.APIKeyEnv != "" {
			seen[endpoint.APIKeyEnv] = struct{}{}
		}
	}
	// The seats that run as agents pay from their own keys, and those keys
	// are named on the launch, not on an endpoint. Left out, the figures a
	// requester reads covered the reception and the design and nothing
	// else - and the implementing and reviewing seats are usually the
	// larger half of a run (reported live 2026-09-17).
	for env := range agentKeyEnvs(config) {
		seen[env] = struct{}{}
	}
	envs := make([]string, 0, len(seen))
	for env := range seen {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	return envs
}

// agentKeyEnvs names every key an agent launch reads, by the seat it pays
// for. An agent names its credential in SecretEnv: the variable the launch
// reads, and the variable this process holds it in.
func agentKeyEnvs(config Config) map[string][]string {
	envs := map[string][]string{}
	add := func(agent AgentConfig, role string) {
		for _, source := range agent.SecretEnv {
			if source != "" {
				envs[source] = append(envs[source], role)
			}
		}
	}
	add(config.Agents.Implementer, "実装")
	if len(config.Agents.ReviewerAgents) == 0 {
		// The shared reviewer launch is used only when no reviewer has its
		// own; a deployment that gives each one a launch never sets this
		// one's key, and asking for it marked every run's figures
		// incomplete (review of #196).
		add(config.Agents.Reviewer, "レビュー")
	}
	if config.Agents.Applier != nil {
		add(*config.Agents.Applier, "設計に沿った実装")
	}
	for _, reviewer := range config.Agents.ReviewerAgents {
		name := reviewer.ReviewerID
		if name == "" {
			name = "review"
		}
		add(reviewer.Agent, "レビュー "+name)
	}
	for _, judge := range config.Agents.DesignReviewerAgents {
		name := judge.ReviewerID
		if name == "" {
			name = "review"
		}
		add(judge.Agent, "設計レビュー "+name)
	}
	return envs
}

// RolesByKeyEnv names the roles each key pays for, so a report can say which
// seats a figure covers instead of printing an environment variable.
func RolesByKeyEnv(config Config) map[string][]string {
	roles := map[string][]string{}
	add := func(endpoint ModelEndpoint, role string) {
		if endpoint.APIKeyEnv == "" {
			return
		}
		roles[endpoint.APIKeyEnv] = append(roles[endpoint.APIKeyEnv], role)
	}
	add(config.Models.Implementer, "実装 (本体からの直接呼び出し)")
	for index, reviewer := range config.Models.Reviewers {
		name := reviewer.ID
		if name == "" {
			name = "review-" + string(rune('a'+index))
		}
		add(reviewer, "レビュー "+name)
	}
	add(config.Models.Readiness.Assessor, "受付")
	add(config.Models.Readiness.Checker, "受付")
	if config.Models.Designer != nil {
		add(*config.Models.Designer, "調査・設計")
	}
	for index, judge := range config.Models.DesignReviewers {
		name := judge.ID
		if name == "" {
			name = "review-" + string(rune('a'+index))
		}
		add(judge, "設計レビュー "+name)
	}
	for env, seats := range agentKeyEnvs(config) {
		roles[env] = append(roles[env], seats...)
	}
	for env, list := range roles {
		roles[env] = dedupeStrings(list)
	}
	return roles
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

// spendBaseURL returns the endpoint every configured role bills through. Roles
// are required to share one gateway (the vendor-host table pins them there), so
// one base URL covers the whole run; an empty result disables spend reporting
// rather than guessing at an address.
func spendBaseURL(config Config) string {
	for _, endpoint := range spendEndpoints(config) {
		if endpoint.BaseURL != "" {
			return endpoint.BaseURL
		}
	}
	return ""
}

func spendEndpoints(config Config) []ModelEndpoint {
	endpoints := []ModelEndpoint{config.Models.Implementer}
	endpoints = append(endpoints, config.Models.Reviewers...)
	endpoints = append(endpoints, config.Models.Readiness.Assessor, config.Models.Readiness.Checker)
	// The investigating designer's roles bill their own keys (#45): the
	// designer and the design judges, when configured.
	if config.Models.Designer != nil {
		endpoints = append(endpoints, *config.Models.Designer)
	}
	endpoints = append(endpoints, config.Models.DesignReviewers...)
	return endpoints
}

// ReadRunSpend totals what every configured key was billed since the run began.
// A key that cannot be read marks the result incomplete and the run continues:
// a billing figure is a line in a report, never a gate on delivering the work.
func ReadRunSpend(ctx context.Context, reader SpendReader, config Config, since time.Time) RunSpend {
	baseURL := spendBaseURL(config)
	envs := SpendKeyEnvs(config)
	if reader == nil || baseURL == "" || since.IsZero() || len(envs) == 0 {
		return RunSpend{}
	}
	spend := RunSpend{Complete: true}
	// Several environment variables routinely hold one key: the default
	// setup gives every seat the same one. Folding them by the key this
	// process actually holds is what keeps one bill from being counted
	// once per variable - the name the gateway answers with cannot do it,
	// because an empty name folds unrelated keys together and a shared name
	// hides distinct ones (review of #196: $3 read as $27, and $18 read as
	// $2).
	for _, group := range foldByHeldKey(envs) {
		key, err := reader.SpendSince(ctx, baseURL, group[0], since)
		if err != nil {
			spend.Complete = false
			continue
		}
		if key.Unpriced > 0 {
			spend.Complete = false
		}
		key.AlsoKeyEnvs = append(key.AlsoKeyEnvs, group[1:]...)
		if key.Approximate {
			spend.Approximate = true
		}
		spend.Keys = append(spend.Keys, key)
		spend.TotalUSD += key.SpendUSD
	}
	if len(spend.Keys) == 0 {
		return RunSpend{}
	}
	// The variables held different values, and the gateway answered with one
	// key's name for more than one of them. Folding them would lose the
	// difference and keeping them apart may count one bill twice, so the
	// total stops claiming to be whole and says so (review of #196).
	named := map[string]struct{}{}
	for _, key := range spend.Keys {
		if key.KeyName == "" {
			continue
		}
		if _, repeated := named[key.KeyName]; repeated {
			spend.Complete = false
			break
		}
		named[key.KeyName] = struct{}{}
	}
	return spend
}

// SpendFXRateUSDJPY converts the billed dollars into yen for the report. It
// matches the gateway console's own fixed rate (FX_RATE_USD_JPY) so a figure in
// a ticket and the same figure on the billing screen agree; the yen amount is a
// reading aid, and the dollar figure stays the authoritative one.
const SpendFXRateUSDJPY = 150

// ComposeSpendText renders the requester-facing cost record. An incomplete
// reading says so instead of printing a total that reads as the whole bill,
// and an empty reading renders nothing at all.
func ComposeSpendText(spend RunSpend, roles map[string][]string) string {
	if len(spend.Keys) == 0 {
		return ""
	}
	var builder strings.Builder
	if spend.Complete {
		builder.WriteString("合計: " + formatSpendUSD(spend.TotalUSD) + " (" + formatSpendJPY(spend.TotalUSD) + ")\n")
	} else {
		builder.WriteString("合計: " + formatSpendUSD(spend.TotalUSD) + " (" + formatSpendJPY(spend.TotalUSD) +
			") — 一部読み取れなかったため、実際の請求はこれより大きくなることがあります\n")
	}
	for _, key := range spend.Keys {
		var names []string
		for _, env := range append([]string{key.KeyEnv}, key.AlsoKeyEnvs...) {
			names = append(names, roles[env]...)
		}
		label := key.KeyName
		if names = dedupeStrings(names); len(names) > 0 {
			label = strings.Join(names, " / ")
		}
		line := "- " + label + ": " + formatSpendUSD(key.SpendUSD) + " (" + formatSpendJPY(key.SpendUSD) + ")"
		if key.Unpriced > 0 {
			line += " ※ " + strconv.Itoa(key.Unpriced) + " 件は金額が確定せず未計上"
		}
		builder.WriteString(line + "\n")
	}
	if spend.Approximate {
		// The difference cannot separate this ticket from anything else
		// billed to the same key while it ran.
		builder.WriteString("この金額は、依頼の開始時と終了時の利用額の差です。" +
			"この間に同じ鍵が使われた分は、別の依頼でも手作業でも、すべてこの金額に含まれます。\n")
	}
	builder.WriteString("為替は 1 ドル " + strconv.Itoa(SpendFXRateUSDJPY) + " 円の固定換算です。\n")
	return builder.String()
}

// formatSpendUSD keeps sub-cent amounts legible: a run that cost $0.0086 must
// not render as "$0.01" when the point of the line is how little it cost.
func formatSpendUSD(amount float64) string {
	switch {
	case amount == 0:
		return "$0"
	case amount < 0.01:
		return "$" + strconv.FormatFloat(amount, 'f', 4, 64)
	default:
		return "$" + strconv.FormatFloat(amount, 'f', 2, 64)
	}
}

func formatSpendJPY(amount float64) string {
	yen := amount * SpendFXRateUSDJPY
	if yen > 0 && yen < 1 {
		return "1 円未満"
	}
	return "約 " + strconv.FormatFloat(yen, 'f', 0, 64) + " 円"
}

// foldByHeldKey groups the variables that hold one and the same key, in the
// order the variables were given. A variable this process does not hold is
// its own group: the read then fails and says the reading is incomplete,
// which is what a missing credential is.
func foldByHeldKey(envs []string) [][]string {
	groups := [][]string{}
	index := map[string]int{}
	for _, env := range envs {
		value := os.Getenv(env)
		if value == "" {
			groups = append(groups, []string{env})
			continue
		}
		if at, ok := index[value]; ok {
			groups[at] = append(groups[at], env)
			continue
		}
		index[value] = len(groups)
		groups = append(groups, []string{env})
	}
	return groups
}
