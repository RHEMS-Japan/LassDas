package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func spendTestConfig() Config {
	config := Config{}
	config.Models.Implementer = ModelEndpoint{
		ID: "hermes-author", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "IMPL_KEY",
	}
	config.Models.Reviewers = []ModelEndpoint{
		{ID: "review-a", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "REVIEW_A_KEY"},
		{ID: "review-b", BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "REVIEW_B_KEY"},
	}
	// 受付は実装役・レビュー A と鍵を共有する (実運用と同じ形)
	config.Models.Readiness.Assessor = ModelEndpoint{BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "IMPL_KEY"}
	config.Models.Readiness.Checker = ModelEndpoint{BaseURL: "https://gateway.example.com/api/v1", APIKeyEnv: "REVIEW_A_KEY"}
	return config
}

type stubSpendReader struct {
	byEnv map[string]KeySpend
	errs  map[string]error
	since time.Time
	calls int
}

func (s *stubSpendReader) SpendSince(_ context.Context, _, apiKeyEnv string, since time.Time) (KeySpend, error) {
	s.calls++
	s.since = since
	if err := s.errs[apiKeyEnv]; err != nil {
		return KeySpend{}, err
	}
	spend := s.byEnv[apiKeyEnv]
	spend.KeyEnv = apiKeyEnv
	return spend, nil
}

func TestSpendKeyEnvsDeduplicatesSharedKeys(t *testing.T) {
	envs := SpendKeyEnvs(spendTestConfig())
	want := []string{"IMPL_KEY", "REVIEW_A_KEY", "REVIEW_B_KEY"}
	if len(envs) != len(want) {
		t.Fatalf("envs = %v, want %v (a key serving two roles must appear once)", envs, want)
	}
	for i := range want {
		if envs[i] != want[i] {
			t.Fatalf("envs = %v, want %v (stable order)", envs, want)
		}
	}
}

func TestRolesByKeyEnvNamesEverySeatOnAKey(t *testing.T) {
	roles := RolesByKeyEnv(spendTestConfig())
	if got := strings.Join(roles["IMPL_KEY"], "|"); got != "実装|受付" {
		t.Errorf("IMPL_KEY roles = %q, want 実装|受付", got)
	}
	if got := strings.Join(roles["REVIEW_B_KEY"], "|"); got != "レビュー review-b" {
		t.Errorf("REVIEW_B_KEY roles = %q, want レビュー review-b", got)
	}
}

func TestReadRunSpendTotalsEveryKey(t *testing.T) {
	reader := &stubSpendReader{byEnv: map[string]KeySpend{
		"IMPL_KEY":     {SpendUSD: 0.84, KeyName: "automation-cheap-gemini"},
		"REVIEW_A_KEY": {SpendUSD: 0.008, KeyName: "automation-cheap-deepseek"},
		"REVIEW_B_KEY": {SpendUSD: 0.005, KeyName: "automation-cheap-luna"},
	}}
	since := time.Now().Add(-30 * time.Minute)

	spend := ReadRunSpend(context.Background(), reader, spendTestConfig(), since)

	if !spend.Complete {
		t.Error("Complete = false, want true when every key was read")
	}
	if got := spend.TotalUSD; got < 0.8529 || got > 0.8531 {
		t.Errorf("TotalUSD = %v, want 0.853", got)
	}
	if reader.calls != 3 {
		t.Errorf("read %d keys, want 3 (one per distinct key, not per role)", reader.calls)
	}
	if !reader.since.Equal(since) {
		t.Errorf("since = %v, want %v", reader.since, since)
	}
}

// 1 つでも読めなければ「不完全」。合計は下限であって請求額ではない。
func TestReadRunSpendMarksIncompleteOnReadFailure(t *testing.T) {
	reader := &stubSpendReader{
		byEnv: map[string]KeySpend{
			"IMPL_KEY":     {SpendUSD: 0.84},
			"REVIEW_B_KEY": {SpendUSD: 0.005},
		},
		errs: map[string]error{"REVIEW_A_KEY": errors.New("gateway down")},
	}

	spend := ReadRunSpend(context.Background(), reader, spendTestConfig(), time.Now().Add(-time.Hour))

	if spend.Complete {
		t.Error("Complete = true, want false when a key could not be read")
	}
	if len(spend.Keys) != 2 {
		t.Errorf("Keys = %d, want the 2 that were readable", len(spend.Keys))
	}
}

// 課金が確定しなかったリクエストがあると、合計は請求より小さい。
func TestReadRunSpendMarksIncompleteOnUnpricedRequests(t *testing.T) {
	reader := &stubSpendReader{byEnv: map[string]KeySpend{
		"IMPL_KEY":     {SpendUSD: 0.84, Unpriced: 3},
		"REVIEW_A_KEY": {SpendUSD: 0.008},
		"REVIEW_B_KEY": {SpendUSD: 0.005},
	}}

	spend := ReadRunSpend(context.Background(), reader, spendTestConfig(), time.Now().Add(-time.Hour))

	if spend.Complete {
		t.Error("Complete = true, want false when the gateway left calls unpriced")
	}
}

func TestReadRunSpendEmptyWithoutReader(t *testing.T) {
	if spend := ReadRunSpend(context.Background(), nil, spendTestConfig(), time.Now()); len(spend.Keys) != 0 {
		t.Error("a run without a reader must report nothing, not zero")
	}
}

func TestComposeSpendTextRendersRolesAndTotal(t *testing.T) {
	config := spendTestConfig()
	spend := RunSpend{Complete: true, TotalUSD: 0.853, Keys: []KeySpend{
		{KeyEnv: "IMPL_KEY", KeyName: "automation-cheap-gemini", SpendUSD: 0.84},
		{KeyEnv: "REVIEW_A_KEY", KeyName: "automation-cheap-deepseek", SpendUSD: 0.008},
		{KeyEnv: "REVIEW_B_KEY", KeyName: "automation-cheap-luna", SpendUSD: 0.005},
	}}

	text := ComposeSpendText(spend, RolesByKeyEnv(config))

	for _, want := range []string{"合計: $0.85", "約 128 円", "実装 / 受付", "レビュー review-b", "$0.0080"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "IMPL_KEY") {
		t.Error("the key's environment variable must not appear in a requester-facing report")
	}
}

// 読み取れなかった時、数字だけを出して「これが全額」と読ませてはいけない。
func TestComposeSpendTextFlagsIncompleteReading(t *testing.T) {
	spend := RunSpend{Complete: false, TotalUSD: 0.84, Keys: []KeySpend{
		{KeyEnv: "IMPL_KEY", SpendUSD: 0.84, KeyName: "k", Unpriced: 2},
	}}

	text := ComposeSpendText(spend, map[string][]string{})

	if !strings.Contains(text, "実際の請求はこれより大きくなることがあります") {
		t.Errorf("an incomplete reading must say so:\n%s", text)
	}
	if !strings.Contains(text, "2 件は金額が確定せず未計上") {
		t.Errorf("unpriced calls must be surfaced:\n%s", text)
	}
}

func TestComposeSpendTextEmptyWhenNothingRead(t *testing.T) {
	if text := ComposeSpendText(RunSpend{}, nil); text != "" {
		t.Errorf("nothing read must render nothing, got %q", text)
	}
}

// 1 セント未満を "$0.01" に丸めると、安さが伝わらないどころか誤りになる。
func TestFormatSpendUSDKeepsSubCentAmounts(t *testing.T) {
	cases := map[float64]string{0: "$0", 0.0086: "$0.0086", 1.234: "$1.23"}
	for amount, want := range cases {
		if got := formatSpendUSD(amount); got != want {
			t.Errorf("formatSpendUSD(%v) = %q, want %q", amount, got, want)
		}
	}
}

func TestGatewaySpendReaderReadsWindowedSpend(t *testing.T) {
	t.Setenv("SPEND_KEY", "csk-test")
	var gotQuery, gotAuth, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("since")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"key_name":"automation-cheap-gemini","spend_usd":0.8612,"unpriced_requests":0}`))
	}))
	defer server.Close()

	reader, err := NewGatewaySpendReader(server.Client())
	if err != nil {
		t.Fatalf("NewGatewaySpendReader: %v", err)
	}
	since := time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC)
	spend, err := reader.SpendSince(context.Background(), server.URL+"/api/v1", "SPEND_KEY", since)
	if err != nil {
		t.Fatalf("SpendSince: %v", err)
	}
	if spend.SpendUSD != 0.8612 || spend.KeyName != "automation-cheap-gemini" {
		t.Errorf("spend = %+v, want 0.8612 / automation-cheap-gemini", spend)
	}
	if gotQuery != "2026-08-27T10:00:00Z" {
		t.Errorf("since query = %q, want RFC3339 UTC", gotQuery)
	}
	if gotAuth != "Bearer csk-test" {
		t.Errorf("auth header = %q, want the bearer key", gotAuth)
	}
	if gotPath != "/api/v1/key/spend" {
		t.Errorf("path = %q, want /api/v1/key/spend", gotPath)
	}
}

// 額の無い応答を 0 と解釈すると「無料だった」と報告してしまう。
func TestGatewaySpendReaderRejectsAmountlessResponse(t *testing.T) {
	t.Setenv("SPEND_KEY", "csk-test")
	for _, payload := range []string{`{"key_name":"k"}`, `{"key_name":"k","spend_usd":-1}`, `not json`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(payload))
		}))
		reader, _ := NewGatewaySpendReader(server.Client())
		_, err := reader.SpendSince(context.Background(), server.URL+"/api/v1", "SPEND_KEY", time.Now().Add(-time.Hour))
		if err == nil {
			t.Errorf("payload %q accepted, want an error rather than a zero", payload)
		}
		server.Close()
	}
}

func TestGatewaySpendReaderFailsOnNonOK(t *testing.T) {
	t.Setenv("SPEND_KEY", "csk-test")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	reader, _ := NewGatewaySpendReader(server.Client())
	if _, err := reader.SpendSince(context.Background(), server.URL+"/api/v1", "SPEND_KEY", time.Now().Add(-time.Hour)); err == nil {
		t.Error("a 503 must surface as an error, not as zero spend")
	}
}

func TestGatewaySpendReaderRequiresKey(t *testing.T) {
	reader, _ := NewGatewaySpendReader(http.DefaultClient)
	if _, err := reader.SpendSince(context.Background(), "https://gateway.example.com/api/v1", "MISSING_KEY_ENV", time.Now()); err == nil {
		t.Error("a missing key must fail rather than call the endpoint unauthenticated")
	}
}

// 実運用では 5 つの環境変数が 3 つの鍵を指す (実装役の鍵が受付も兼ねる等)。
// 変数名で数えると同じ鍵を 2 度読んで合計が倍近くになる。
// 実際の走行で合計がおよそ 2 倍に報告された欠陥の回帰テスト。
func TestReadRunSpendCountsSharedKeyOnce(t *testing.T) {
	config := spendTestConfig()
	// 受付は実装役/レビュー A と「別の変数名」で「同じ鍵」を使う
	config.Models.Readiness.Assessor.APIKeyEnv = "READINESS_IMPL_KEY"
	config.Models.Readiness.Checker.APIKeyEnv = "READINESS_REVIEW_KEY"

	reader := &stubSpendReader{byEnv: map[string]KeySpend{
		"IMPL_KEY":             {SpendUSD: 0.92, KeyName: "automation-cheap-gemini"},
		"READINESS_IMPL_KEY":   {SpendUSD: 0.92, KeyName: "automation-cheap-gemini"},
		"REVIEW_A_KEY":         {SpendUSD: 0.0018, KeyName: "automation-cheap-deepseek"},
		"READINESS_REVIEW_KEY": {SpendUSD: 0.0018, KeyName: "automation-cheap-deepseek"},
		"REVIEW_B_KEY":         {SpendUSD: 0, KeyName: "automation-cheap-luna"},
	}}

	spend := ReadRunSpend(context.Background(), reader, config, time.Now().Add(-time.Hour))

	if got := spend.TotalUSD; got < 0.9217 || got > 0.9219 {
		t.Errorf("TotalUSD = %v, want 0.9218 (a shared key must be billed once, not twice)", got)
	}
	if len(spend.Keys) != 3 {
		t.Errorf("Keys = %d, want 3 distinct keys behind 5 variables", len(spend.Keys))
	}
}

// 兼務している鍵の行には、その鍵が担った役をすべて並べる。
func TestComposeSpendTextFoldsRolesOfASharedKey(t *testing.T) {
	config := spendTestConfig()
	config.Models.Readiness.Assessor.APIKeyEnv = "READINESS_IMPL_KEY"
	config.Models.Readiness.Checker.APIKeyEnv = "READINESS_REVIEW_KEY"

	spend := RunSpend{Complete: true, TotalUSD: 0.9218, Keys: []KeySpend{
		{KeyEnv: "IMPL_KEY", KeyName: "automation-cheap-gemini", SpendUSD: 0.92, AlsoKeyEnvs: []string{"READINESS_IMPL_KEY"}},
		{KeyEnv: "REVIEW_A_KEY", KeyName: "automation-cheap-deepseek", SpendUSD: 0.0018, AlsoKeyEnvs: []string{"READINESS_REVIEW_KEY"}},
		{KeyEnv: "REVIEW_B_KEY", KeyName: "automation-cheap-luna", SpendUSD: 0},
	}}

	text := ComposeSpendText(spend, RolesByKeyEnv(config))

	if !strings.Contains(text, "実装 / 受付:") {
		t.Errorf("a key serving two seats must list both:\n%s", text)
	}
	if !strings.Contains(text, "レビュー review-a / 受付:") {
		t.Errorf("the review key that also does readiness must list both:\n%s", text)
	}
	if strings.Count(text, "automation-cheap-gemini") > 0 {
		t.Errorf("a named role should replace the raw key name:\n%s", text)
	}
}
