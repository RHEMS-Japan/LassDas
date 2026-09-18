package initwizard

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The engine speaks OpenAI-compatible chat completions, so a provider is a
// base URL. Until 2026-09-18 the setup pinned OpenRouter and refused every
// other value, which meant a discounted marketplace could not be used at all
// even though the runtime could already talk to it.
func TestTheProviderIsChosenAndTheKeysGoThere(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.BaseURL, s.ModelKeyMode = "", modelKeysShared
	for _, role := range allRoles(s) {
		endpoint := s.Models[role]
		endpoint.BaseURL = ""
		s.Models[role] = endpoint
	}
	// Option 1 is the second named provider.
	ui := &fakeUI{approve: true, picks: []int{1}}
	hosts := map[string]bool{}
	w := Wizard{UI: ui, API: API{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		hosts[req.URL.Host] = true
		return response(200, map[string]any{"id": "chatcmpl-fixture", "choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"status":"ready"}`}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}), nil
	})}}}
	if err := w.models(context.Background(), s, secrets); err != nil {
		t.Fatal(err)
	}
	want := ModelProviders[1].BaseURL
	if s.BaseURL != want {
		t.Fatalf("接続先 = %q, want %q", s.BaseURL, want)
	}
	for _, role := range allRoles(s) {
		if s.Models[role].BaseURL != want {
			t.Fatalf("%s が %q を向いています", role, s.Models[role].BaseURL)
		}
	}
	if len(hosts) != 1 || !hosts["api.cheaperinference.com"] {
		t.Fatalf("鍵が送られた先: %v", hosts)
	}
	// The choice is offered with both names, and says what changes.
	if len(ui.offered) == 0 {
		t.Fatal("接続先が一覧で聞かれていません")
	}
	joined := ""
	for _, option := range ui.offered[0] {
		joined += option.Label + " / " + option.Detail + "\n"
	}
	for _, want := range []string{"OpenRouter", "Cheaper Inference", "その他", "提供会社"} {
		if !strings.Contains(joined, want) {
			t.Errorf("接続先の選択肢に %q がありません:\n%s", want, joined)
		}
	}
}

// A base URL is where a credential is sent. http, a stray slash or a
// host-less value is refused before any key is stored against it.
func TestAConnectionTargetThatCannotBeOneIsRefused(t *testing.T) {
	for value, ok := range map[string]bool{
		"https://api.cheaperinference.com/v1": true,
		"https://openrouter.ai/api/v1":        true,
		"http://api.example.com/v1":           false,
		"https://api.example.com/v1/":         false,
		"":                                    false,
		"api.example.com/v1":                  false,
		" https://api.example.com/v1":         false,
		"https://api.example.com/v1?key=x":    false,
	} {
		err := CheckModelBaseURL(value)
		if ok && err != nil {
			t.Errorf("%q は受け付けるべきです: %v", value, err)
		}
		if !ok && err == nil {
			t.Errorf("%q を受け付けました", value)
		}
	}
}

// The provider's name travels into what a person is asked for ("… の API
// キー"), so it must never say OpenRouter about somewhere else.
func TestTheProviderNameFollowsTheBaseURL(t *testing.T) {
	if name := ProviderName(ModelProviders[1].BaseURL); name != "Cheaper Inference" {
		t.Errorf("name = %q", name)
	}
	if name := ProviderName("https://models.example.com/v1"); name != "models.example.com" {
		t.Errorf("未知の接続先は host で呼ぶ: %q", name)
	}
	if VendorDerivable(ModelProviders[1].BaseURL) {
		t.Error("会社名が入らない名前の接続先で、提供会社を名前から判定できることになっています")
	}
	if !VendorDerivable(openRouterBaseURL) {
		t.Error("会社/モデル の形の接続先で判定できないことになっています")
	}
}
