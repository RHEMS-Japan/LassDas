package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// The engine holds no budget of its own. What stops a delivery that would
// otherwise go on for ever is the spending limit on the provider's key: the
// provider refuses, the engine reports it and waits, and the operator
// raises the limit or waits for its reset.
//
// That makes a key with no limit the one way a running engine can quietly
// spend without bound, and the setup is where it can still be said. This
// reads the limit through the same GET {baseURL}/key the spend reading
// already uses, so a provider that answers one answers the other.

// KeyLimit is what the provider will let one key spend before it refuses.
type KeyLimit struct {
	// LimitUSD is nil where the key has no limit at all — which the
	// provider states as an absent number, never as a zero.
	LimitUSD *float64
	// RemainingUSD is what is left of it, where the provider says.
	RemainingUSD *float64
}

// Limited reports whether a limit is set on the key.
func (k KeyLimit) Limited() bool { return k.LimitUSD != nil }

// ReadKeyLimit asks the gateway about the calling key. The key is passed by
// value rather than by variable name: this is read during setup, where the
// keys are in a file the person typed them into and not yet in anyone's
// environment.
//
// The key itself never appears in what comes back out of here, errors
// included: a setup check prints what it is told.
func ReadKeyLimit(ctx context.Context, client *http.Client, baseURL, apiKey string) (KeyLimit, error) {
	if ctx == nil || baseURL == "" {
		return KeyLimit{}, errors.New("key limit request is invalid")
	}
	if apiKey == "" || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, "\r\n\x00") {
		return KeyLimit{}, errors.New("key limit needs a usable API key")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/key", nil)
	if err != nil {
		return KeyLimit{}, errors.New("key limit request could not be built")
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	response, err := client.Do(request)
	if err != nil {
		return KeyLimit{}, errors.New("the provider could not be reached")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return KeyLimit{}, errors.New("the provider answered " + response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxSpendResponseBytes+1))
	if err != nil || len(body) > MaxSpendResponseBytes {
		return KeyLimit{}, errors.New("the provider's answer could not be read")
	}
	var parsed struct {
		Data struct {
			Limit     *float64 `json:"limit"`
			Remaining *float64 `json:"limit_remaining"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return KeyLimit{}, errors.New("the provider's answer is not the expected shape")
	}
	// A negative limit is not a limit, and reading one as set would let a
	// key with a broken answer pass the check that exists to catch exactly
	// this.
	if parsed.Data.Limit != nil && *parsed.Data.Limit < 0 {
		parsed.Data.Limit = nil
	}
	return KeyLimit{LimitUSD: parsed.Data.Limit, RemainingUSD: parsed.Data.Remaining}, nil
}
