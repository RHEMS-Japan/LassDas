package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func decisionEndpoint(baseURL string) ModelEndpoint {
	return ModelEndpoint{ID: "decider", Vendor: "TypeSafe", Model: "typesafe/jev-1.13",
		BaseURL: baseURL, APIKeyEnv: "DECISION_KEY"}
}

// The decision model is reached beside the chat completions this engine
// already posts to, with the same key: nothing new is configured to use one.
func TestADecisionGoesToTheGatewayThisInstanceAlreadyUses(t *testing.T) {
	var path, auth, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = io.WriteString(w, `{"is_answer":{"type":"noul","noul":0.97,"confidence":0.9}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "artificial-decision-key")

	client, err := NewSystemOneClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	answers, err := client.Decide(context.Background(), decisionEndpoint(server.URL), SystemOneRequest{
		Model: "typesafe/jev-1.13", State: "この依頼は取り消します",
		Questions: map[string]SystemOneQuestion{
			"is_answer": {Kind: SystemOneNoul, Instructions: "The comment answers the question that was asked"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/systemone" {
		t.Errorf("送り先 = %q", path)
	}
	if auth != "Bearer artificial-decision-key" {
		t.Errorf("鍵が届いていません: %q", auth)
	}
	var sent SystemOneRequest
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("送った本文が読めません: %v", err)
	}
	if sent.Model != "typesafe/jev-1.13" || sent.State == "" || len(sent.Questions) != 1 {
		t.Fatalf("送った本文 = %+v", sent)
	}
	got := answers["is_answer"]
	if got.Noul == nil || *got.Noul != 0.97 {
		t.Fatalf("確率が読めていません: %+v", got)
	}
	if got.Confidence == nil || *got.Confidence != 0.9 {
		t.Fatalf("確信度が読めていません: %+v", got)
	}
}

// A probability that is absent is not a probability of zero: read as zero it
// would say "certainly not" about a question nobody answered. The same for a
// choice with no value and a score with none.
func TestAnAnswerWithNoValueIsRefusedRatherThanReadAsZero(t *testing.T) {
	for name, reply := range map[string]string{
		"noul なし":   `{"q":{"type":"noul"}}`,
		"確率の範囲外":    `{"q":{"type":"noul","noul":1.4}}`,
		"答えが返っていない": `{"other":{"type":"noul","noul":0.5}}`,
		"JSON ではない": `<html>gateway error</html>`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, reply)
		}))
		t.Setenv("DECISION_KEY", "k")
		client, _ := NewSystemOneClient(server.Client())
		_, err := client.Decide(context.Background(), decisionEndpoint(server.URL), SystemOneRequest{
			Model: "m", State: "s",
			Questions: map[string]SystemOneQuestion{"q": {Kind: SystemOneNoul, Instructions: "i"}},
		})
		if err == nil {
			t.Errorf("%s が受け入れられました", name)
		}
		server.Close()
	}
}

// The gateway's own refusal reaches the operator. A key that is not allowed
// this model answers 403 with that sentence, and hiding it leaves nothing to
// act on - the failure this engine spent today removing everywhere else.
func TestTheGatewaysRefusalIsSaidOutLoud(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Model not allowed for this key"}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	_, err := client.Decide(context.Background(), decisionEndpoint(server.URL), SystemOneRequest{
		Model: "m", State: "s",
		Questions: map[string]SystemOneQuestion{"q": {Kind: SystemOneNoul, Instructions: "i"}},
	})
	if err == nil {
		t.Fatal("403 が成功として扱われました")
	}
	for _, want := range []string{"403", "Model not allowed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("断られた理由に %q がありません: %v", want, err)
		}
	}
}

// The two shapes of criteria differ, and sending the wrong one is answered
// by the gateway with its own 400 - which reached the run as "the decision
// could not be read". The call is refused here instead, naming the question.
func TestAQuestionBuiltWrongIsRefusedBeforeItIsSent(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())

	for name, question := range map[string]SystemOneQuestion{
		"choice なのに一覧": {Kind: SystemOneChoice, Instructions: "i", Criteria: []string{"a", "b"}},
		"score なのに対応表": {Kind: SystemOneScore, Instructions: "i", Criteria: map[string]string{"a": "b"}},
		"noul なのに条件つき": {Kind: SystemOneNoul, Instructions: "i", Criteria: []string{"a"}},
		"聞きたいことが空":     {Kind: SystemOneNoul, Instructions: "  "},
		"型が知らないもの":     {Kind: "verdict", Instructions: "i"},
	} {
		_, err := client.Decide(context.Background(), decisionEndpoint(server.URL), SystemOneRequest{
			Model: "m", State: "s", Questions: map[string]SystemOneQuestion{"q1": question},
		})
		if err == nil {
			t.Errorf("%s が送られました", name)
		} else if !strings.Contains(err.Error(), "q1") {
			t.Errorf("%s: どの質問が悪いか言っていません: %v", name, err)
		}
	}
	if sent {
		t.Error("形の悪い要求が gateway まで送られました")
	}
}

// The state is bounded by the engine, inside the model's own window, so a
// caller whose input grew is told here rather than by the gateway.
func TestAStateTooLargeIsRefusedWithItsSize(t *testing.T) {
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(http.DefaultClient)
	_, err := client.Decide(context.Background(), decisionEndpoint("https://gateway.example/v1"), SystemOneRequest{
		Model: "m", State: strings.Repeat("x", MaxSystemOneStateBytes+1),
		Questions: map[string]SystemOneQuestion{"q": {Kind: SystemOneNoul, Instructions: "i"}},
	})
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("大きすぎる状態が通りました: %v", err)
	}
}

// Several questions travel in one call: they are evaluated in parallel
// against the same state, so asking four costs one round trip.
func TestSeveralQuestionsTravelInOneCall(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"a":{"type":"noul","noul":0.1},"b":{"type":"noul","noul":0.2},`+
			`"c":{"type":"choice","choice":"yes"},"d":{"type":"score","score":1.4}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	answers, err := client.Decide(context.Background(), decisionEndpoint(server.URL), SystemOneRequest{
		Model: "m", State: "s",
		Questions: map[string]SystemOneQuestion{
			"a": {Kind: SystemOneNoul, Instructions: "i"},
			"b": {Kind: SystemOneNoul, Instructions: "i"},
			"c": {Kind: SystemOneChoice, Instructions: "i", Criteria: map[string]string{"yes": "y", "no": "n"}},
			"d": {Kind: SystemOneScore, Instructions: "i", Criteria: []string{"low", "high"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("往復が %d 回", calls)
	}
	if len(answers) != 4 || answers["c"].Choice == nil || *answers["c"].Choice != "yes" || answers["d"].Score == nil {
		t.Fatalf("answers = %+v", answers)
	}
}
