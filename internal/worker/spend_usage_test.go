package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The reply shape of the endpoint every OpenAI-compatible gateway has. The
// label is in it and must not come back out: it is whatever a person typed
// when they made the key, and the cost line is a requester-facing comment.
func TestAKeysRunningTotalIsReadAndItsLabelIsNot(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key" {
			t.Errorf("asked for %q", r.URL.Path)
		}
		authorization = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"data":{"label":"sk-or-v1-0123456789abcdef","usage":3.5,"is_free_tier":false,"limit":null}}`)
	}))
	defer server.Close()
	t.Setenv("SPEND_KEY", "sk-or-v1-0123456789abcdef")

	reader, err := NewGatewayUsageReader(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	usage, err := reader.KeyUsage(context.Background(), server.URL, "SPEND_KEY")
	if err != nil {
		t.Fatalf("the running total could not be read: %v", err)
	}
	if usage.UsageUSD != 3.5 || usage.KeyEnv != "SPEND_KEY" {
		t.Fatalf("usage = %+v", usage)
	}
	if authorization != "Bearer sk-or-v1-0123456789abcdef" {
		t.Errorf("the key did not reach the endpoint: %q", authorization)
	}
}

// A reply with no number is not a reply of zero. Read as zero at the run's
// start, a key's whole lifetime would be charged to one ticket.
func TestAMissingRunningTotalIsRefusedRatherThanReadAsZero(t *testing.T) {
	for name, body := range map[string]string{
		"no usage field": `{"data":{"label":"x"}}`,
		"null usage":     `{"data":{"usage":null}}`,
		"negative":       `{"data":{"usage":-1}}`,
		"not json":       `<html>gateway error</html>`,
		"empty":          ``,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		}))
		t.Setenv("SPEND_KEY", "k")
		reader, err := NewGatewayUsageReader(server.Client())
		if err != nil {
			t.Fatal(err)
		}
		if usage, err := reader.KeyUsage(context.Background(), server.URL, "SPEND_KEY"); err == nil {
			t.Errorf("%s was accepted as %v", name, usage.UsageUSD)
		}
		server.Close()
	}
}

// What one ticket cost, from a provider that can only say what a key has
// been billed in total.
func TestTheDifferenceBetweenTwoReadingsIsWhatTheRunCost(t *testing.T) {
	reader := &fakeUsage{values: map[string]float64{"SPEND_KEY": 12.25}}
	baseline := UsageBaseline{TakenAt: time.Now().UTC(), Keys: []KeyUsage{{KeyEnv: "SPEND_KEY", UsageUSD: 12.0}}}
	delta, err := NewUsageDeltaReader(reader, baseline)
	if err != nil {
		t.Fatal(err)
	}
	spend, err := delta.SpendSince(context.Background(), "https://gateway.example", "SPEND_KEY", time.Now())
	if err != nil {
		t.Fatalf("the difference could not be read: %v", err)
	}
	if spend.SpendUSD != 0.25 {
		t.Fatalf("spend = %v, want 0.25", spend.SpendUSD)
	}
	if !spend.Approximate {
		t.Error("a difference of running totals was not marked as one")
	}
}

// A key nobody read at the start has no difference to take. Answering zero
// would print "this ticket cost nothing".
func TestAKeyWithNoStartingReadingHasNoAnswer(t *testing.T) {
	reader := &fakeUsage{values: map[string]float64{"OTHER_KEY": 5}}
	baseline := UsageBaseline{TakenAt: time.Now().UTC(), Keys: []KeyUsage{{KeyEnv: "OTHER_KEY", UsageUSD: 5}}}
	delta, err := NewUsageDeltaReader(reader, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delta.SpendSince(context.Background(), "https://gateway.example", "SPEND_KEY", time.Now()); err == nil {
		t.Fatal("a key with no starting reading was given a figure")
	}
}

// The key was replaced, or the provider reset its count. A negative
// difference is not a refund.
func TestARunningTotalThatWentBackwardsIsRefused(t *testing.T) {
	reader := &fakeUsage{values: map[string]float64{"SPEND_KEY": 1}}
	baseline := UsageBaseline{TakenAt: time.Now().UTC(), Keys: []KeyUsage{{KeyEnv: "SPEND_KEY", UsageUSD: 9}}}
	delta, err := NewUsageDeltaReader(reader, baseline)
	if err != nil {
		t.Fatal(err)
	}
	if spend, err := delta.SpendSince(context.Background(), "https://gateway.example", "SPEND_KEY", time.Now()); err == nil {
		t.Fatalf("a backwards reading was reported as %v", spend.SpendUSD)
	}
}

// Variables that hold one and the same key are read once and recorded under
// every one of their names, so the subtraction finds the key whichever
// variable the later reading names.
func TestOneKeyUnderTwoNamesIsReadOnceAndFoundUnderBoth(t *testing.T) {
	t.Setenv("IMPL_KEY", "same-key")
	t.Setenv("REVIEW_KEY", "same-key")
	reader := &fakeUsage{values: map[string]float64{"IMPL_KEY": 4, "REVIEW_KEY": 4}}
	config := twoSeatConfig("https://gateway.example")

	baseline := ReadUsageBaseline(context.Background(), reader, config)
	if reader.calls != 1 {
		t.Fatalf("the same key was read %d times", reader.calls)
	}
	for _, env := range []string{"IMPL_KEY", "REVIEW_KEY"} {
		if usage, known := baseline.usage(env); !known || usage != 4 {
			t.Errorf("%s: usage=%v known=%v", env, usage, known)
		}
	}
}

// The precise per-window reading is the better answer and is asked for
// first; the difference answers only when it cannot.
func TestThePreciseReadingWinsAndTheDifferenceCatchesTheRest(t *testing.T) {
	precise := spendFunc(func(string) (KeySpend, error) {
		return KeySpend{KeyEnv: "SPEND_KEY", SpendUSD: 0.4}, nil
	})
	difference := spendFunc(func(string) (KeySpend, error) {
		return KeySpend{KeyEnv: "SPEND_KEY", SpendUSD: 9.9, Approximate: true}, nil
	})
	chained, err := NewFallbackSpendReader(precise, difference)
	if err != nil {
		t.Fatal(err)
	}
	spend, err := chained.SpendSince(context.Background(), "https://gateway.example", "SPEND_KEY", time.Now())
	if err != nil || spend.SpendUSD != 0.4 || spend.Approximate {
		t.Fatalf("spend = %+v err = %v", spend, err)
	}

	refusing := spendFunc(func(string) (KeySpend, error) { return KeySpend{}, errors.New("404") })
	chained, err = NewFallbackSpendReader(refusing, difference)
	if err != nil {
		t.Fatal(err)
	}
	spend, err = chained.SpendSince(context.Background(), "https://gateway.example", "SPEND_KEY", time.Now())
	if err != nil || spend.SpendUSD != 9.9 || !spend.Approximate {
		t.Fatalf("spend = %+v err = %v", spend, err)
	}
}

// A requester reading a difference is told it is one, because another
// delivery on the same key is inside it.
func TestTheCostLineSaysWhenTheFigureIsADifference(t *testing.T) {
	roles := map[string][]string{"SPEND_KEY": {"実装"}}
	exact := ComposeSpendText(RunSpend{Complete: true, TotalUSD: 0.4,
		Keys: []KeySpend{{KeyEnv: "SPEND_KEY", SpendUSD: 0.4}}}, roles)
	if strings.Contains(exact, "差です") {
		t.Errorf("a per-window figure was described as a difference: %q", exact)
	}
	approximate := ComposeSpendText(RunSpend{Complete: true, TotalUSD: 0.4, Approximate: true,
		Keys: []KeySpend{{KeyEnv: "SPEND_KEY", SpendUSD: 0.4, Approximate: true}}}, roles)
	if !strings.Contains(approximate, "差です") || !strings.Contains(approximate, "別の依頼") {
		t.Errorf("a difference was printed as though it were exact: %q", approximate)
	}
	// The difference holds everything billed to that key inside the
	// window, not only what ran at the same time. Saying "at the same
	// time" would let a requester rule out a delivery that ran and
	// finished inside the window, which is in the number (review of #202).
	if strings.Contains(approximate, "同時") {
		t.Errorf("the line limits the figure to concurrent work, which is narrower than what it holds: %q", approximate)
	}
	if !strings.Contains(approximate, "すべてこの金額に含まれます") {
		t.Errorf("the line does not say everything billed in the window is inside it: %q", approximate)
	}
}

// A run whose figures are differences says so in the total it keeps too.
func TestARunWhoseFiguresAreDifferencesIsMarkedAsSuch(t *testing.T) {
	t.Setenv("IMPL_KEY", "same-key")
	t.Setenv("REVIEW_KEY", "same-key")
	reader := &fakeUsage{values: map[string]float64{"IMPL_KEY": 7.5}}
	baseline := UsageBaseline{TakenAt: time.Now().UTC(), Keys: []KeyUsage{
		{KeyEnv: "IMPL_KEY", UsageUSD: 7.0}, {KeyEnv: "REVIEW_KEY", UsageUSD: 7.0},
	}}
	delta, err := NewUsageDeltaReader(reader, baseline)
	if err != nil {
		t.Fatal(err)
	}
	spend := ReadRunSpend(context.Background(), delta, twoSeatConfig("https://gateway.example"), time.Now().Add(-time.Hour))
	if !spend.Approximate {
		t.Fatal("the run's figures were differences and it did not say so")
	}
	if spend.TotalUSD != 0.5 {
		t.Fatalf("total = %v, want 0.5 (one key, read once)", spend.TotalUSD)
	}
}

// twoSeatConfig configures an implementer and one reviewer against the same
// gateway, each naming its own key variable.
func twoSeatConfig(baseURL string) Config {
	config := Config{}
	config.Models.Implementer = ModelEndpoint{BaseURL: baseURL, APIKeyEnv: "IMPL_KEY", Model: "m"}
	config.Models.Reviewers = []ModelEndpoint{{ID: "review-a", BaseURL: baseURL, APIKeyEnv: "REVIEW_KEY", Model: "m"}}
	return config
}

type fakeUsage struct {
	values map[string]float64
	calls  int
}

func (f *fakeUsage) KeyUsage(_ context.Context, _, apiKeyEnv string) (KeyUsage, error) {
	f.calls++
	value, known := f.values[apiKeyEnv]
	if !known {
		return KeyUsage{}, errors.New("no such key")
	}
	return KeyUsage{KeyEnv: apiKeyEnv, UsageUSD: value}, nil
}

type spendFunc func(apiKeyEnv string) (KeySpend, error)

func (f spendFunc) SpendSince(_ context.Context, _, apiKeyEnv string, _ time.Time) (KeySpend, error) {
	return f(apiKeyEnv)
}
