package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A 429 is asked again, so the one that ends the call is usually not the
// first. The count of attempts used to be tested before the status, which
// dropped the classification for every 429 after the first — and the
// reception note keyed off it then told an exhausted balance that running
// the same ticket again was worth doing. Measured end to end in the review
// of #133: the ticket said "temporary congestion" while the balance was
// spent.
func TestALimitStaysALimitAfterTheFirstAnswer(t *testing.T) {
	t.Setenv("LASSDAS_TEST_KEY", "k")
	for _, tc := range []struct {
		name       string
		retryAfter string
		wantPhrase string
	}{
		{"no Retry-After at all", "", LimitNotLiftedPhrase},
		{"a Retry-After longer than a turn waits", "600", RetryAfterTooLongPhrase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var answers atomic.Int64
			limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if answers.Add(1) == 1 {
					// The first answer is one the ladder waits on, so the
					// call reaches its second with attempts already spent.
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusTooManyRequests)
					return
				}
				if tc.retryAfter != "" {
					w.Header().Set("Retry-After", tc.retryAfter)
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer limited.Close()
			client, err := NewGatewayClient(&http.Client{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			_, callErr := client.ChatCompletions(context.Background(),
				ModelEndpoint{Model: "m", BaseURL: limited.URL, APIKeyEnv: "LASSDAS_TEST_KEY"}, ChatRequest{Model: "m"})
			if callErr == nil {
				t.Fatal("a limited gateway reported an answer")
			}
			if answers.Load() < 2 {
				t.Fatalf("the call ended on its first answer (%d): the case this holds never happened", answers.Load())
			}
			if !strings.Contains(callErr.Error(), tc.wantPhrase) {
				t.Fatalf("the limit was lost once there was a count of attempts: %q", callErr.Error())
			}
			// The count survives too — it is not the classification's
			// replacement, it is beside it.
			if !strings.Contains(callErr.Error(), AttemptsExhaustedPhrase) {
				t.Fatalf("the count of attempts was dropped: %q", callErr.Error())
			}
		})
	}
}
