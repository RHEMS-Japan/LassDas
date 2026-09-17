package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// KeyUsage is one key's running total: everything that key has ever been
// billed, as the provider reports it now. On its own it says nothing about a
// ticket; the difference between two readings does.
type KeyUsage struct {
	KeyEnv   string  `json:"key_env"`
	UsageUSD float64 `json:"usage_usd"`
}

// UsageReader reads a key's running total.
type UsageReader interface {
	KeyUsage(ctx context.Context, baseURL, apiKeyEnv string) (KeyUsage, error)
}

// GatewayUsageReader calls GET {baseURL}/key, which answers about the calling
// key and nothing else. Every OpenAI-compatible gateway this engine talks to
// has it, including providers with no per-window billing endpoint at all -
// which is why a run against one of those could report no cost whatsoever
// before this existed.
//
// The reply's label is deliberately not read. It is whatever a person typed
// when they made the key, it travels into a requester-facing comment, and the
// report has the configured role names to print instead.
type GatewayUsageReader struct {
	client *http.Client
}

func NewGatewayUsageReader(client *http.Client) (*GatewayUsageReader, error) {
	if client == nil {
		return nil, errors.New("HTTP client is required")
	}
	return &GatewayUsageReader{client: client}, nil
}

func (g *GatewayUsageReader) KeyUsage(ctx context.Context, baseURL, apiKeyEnv string) (KeyUsage, error) {
	if g == nil || g.client == nil || ctx == nil {
		return KeyUsage{}, safeModelError("usage transport is invalid")
	}
	if baseURL == "" {
		return KeyUsage{}, safeModelError("usage request is invalid")
	}
	apiKey := os.Getenv(apiKeyEnv)
	if apiKeyEnv == "" || apiKey == "" || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, "\r\n\x00") {
		return KeyUsage{}, safeModelError("usage API key is unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/key", nil)
	if err != nil {
		return KeyUsage{}, safeModelError("usage request could not be built")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := g.client.Do(request)
	if err != nil {
		return KeyUsage{}, safeModelError("usage request failed: " + err.Error())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return KeyUsage{}, safeModelError("usage endpoint returned " + response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxSpendResponseBytes+1))
	if err != nil || len(body) > MaxSpendResponseBytes {
		return KeyUsage{}, safeModelError("usage response could not be read")
	}
	var parsed struct {
		Data struct {
			Usage *float64 `json:"usage"`
		} `json:"data"`
	}
	// An absent number is not a zero. Read as zero at the start of a run it
	// would make the whole of a key's lifetime look like this ticket's cost.
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Data.Usage == nil || *parsed.Data.Usage < 0 {
		return KeyUsage{}, safeModelError("usage response is invalid")
	}
	return KeyUsage{KeyEnv: apiKeyEnv, UsageUSD: *parsed.Data.Usage}, nil
}

// UsageBaseline is every configured key's running total at the moment a run
// began. It is written beside the run, because the reading that subtracts it
// happens in another process minutes or hours later.
type UsageBaseline struct {
	TakenAt time.Time  `json:"taken_at"`
	Keys    []KeyUsage `json:"keys"`
}

// Usable reports whether the baseline can answer for anything.
func (b UsageBaseline) Usable() bool { return len(b.Keys) > 0 && !b.TakenAt.IsZero() }

func (b UsageBaseline) usage(keyEnv string) (float64, bool) {
	for _, key := range b.Keys {
		if key.KeyEnv == keyEnv {
			return key.UsageUSD, true
		}
	}
	return 0, false
}

// ReadUsageBaseline reads every configured key's running total once, before
// the run's first paid call. Variables holding one and the same key are read
// once and recorded under each of their names, so the subtraction later finds
// the key it is asked about whichever variable names it.
//
// A key that cannot be read is simply absent: the run starts either way, and
// the report says the cost is not available rather than inventing one.
func ReadUsageBaseline(ctx context.Context, reader UsageReader, config Config) UsageBaseline {
	baseURL := spendBaseURL(config)
	envs := SpendKeyEnvs(config)
	if reader == nil || baseURL == "" || len(envs) == 0 {
		return UsageBaseline{}
	}
	baseline := UsageBaseline{TakenAt: time.Now().UTC()}
	for _, group := range foldByHeldKey(envs) {
		usage, err := reader.KeyUsage(ctx, baseURL, group[0])
		if err != nil {
			continue
		}
		for _, env := range group {
			baseline.Keys = append(baseline.Keys, KeyUsage{KeyEnv: env, UsageUSD: usage.UsageUSD})
		}
	}
	if len(baseline.Keys) == 0 {
		return UsageBaseline{}
	}
	return baseline
}

// UsageDeltaReader answers what a key was billed for this run by subtracting
// the running total taken when the run began from the one it reads now. It is
// the reading for a provider that has no per-window billing endpoint, which
// is most of them.
//
// What it cannot do is tell this run's spend apart from anything else billed
// to the same key while it ran: another delivery on the same key, or a person
// using it by hand, lands inside the difference. The figures it returns say
// so, and the requester-facing line says so in words.
type UsageDeltaReader struct {
	reader   UsageReader
	baseline UsageBaseline
}

func NewUsageDeltaReader(reader UsageReader, baseline UsageBaseline) (*UsageDeltaReader, error) {
	if reader == nil {
		return nil, errors.New("a usage reader is required")
	}
	if !baseline.Usable() {
		return nil, errors.New("a baseline taken at the run's start is required")
	}
	return &UsageDeltaReader{reader: reader, baseline: baseline}, nil
}

// SpendSince ignores the window it is given: this reading's window is the
// baseline, taken when the run began, which is the same moment.
func (u *UsageDeltaReader) SpendSince(ctx context.Context, baseURL, apiKeyEnv string, _ time.Time) (KeySpend, error) {
	if u == nil || u.reader == nil {
		return KeySpend{}, safeModelError("usage transport is invalid")
	}
	before, known := u.baseline.usage(apiKeyEnv)
	if !known {
		return KeySpend{}, safeModelError("this key had no reading when the run began")
	}
	now, err := u.reader.KeyUsage(ctx, baseURL, apiKeyEnv)
	if err != nil {
		return KeySpend{}, err
	}
	if now.UsageUSD < before {
		// The key was replaced, or the provider reset its count. The
		// difference is not this run's cost, and a negative one is not a
		// refund.
		return KeySpend{}, safeModelError("the key's running total went backwards since the run began")
	}
	return KeySpend{KeyEnv: apiKeyEnv, SpendUSD: now.UsageUSD - before, Approximate: true}, nil
}

// FallbackSpendReader asks the precise reading first and the difference only
// when it cannot answer. A gateway that bills per window knows exactly what
// this run cost; a provider that does not answers 404 and the difference is
// the best true thing left.
type FallbackSpendReader struct {
	first  SpendReader
	second SpendReader
}

func NewFallbackSpendReader(first, second SpendReader) (*FallbackSpendReader, error) {
	if first == nil || second == nil {
		return nil, errors.New("both readings are required")
	}
	return &FallbackSpendReader{first: first, second: second}, nil
}

func (f *FallbackSpendReader) SpendSince(ctx context.Context, baseURL, apiKeyEnv string, since time.Time) (KeySpend, error) {
	if f == nil || f.first == nil || f.second == nil {
		return KeySpend{}, safeModelError("spend transport is invalid")
	}
	spend, err := f.first.SpendSince(ctx, baseURL, apiKeyEnv, since)
	if err == nil {
		return spend, nil
	}
	return f.second.SpendSince(ctx, baseURL, apiKeyEnv, since)
}
