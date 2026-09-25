package decisions

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// theKey is a value distinctive enough that finding it anywhere it does not
// belong is unambiguous.
const theKey = "sk-test-DO-NOT-LEAK-6f2c19a4"

const keyVariable = "DECISIONS_TEST_KEY"

// answered is the reply shape the service documents: the answers under
// their own key, one per question, beside the record of what answered and
// what it cost. The engine is held to this shape rather than to a map of
// answers at the top level, because reading the wrong one back turns every
// live call into "the answer could not be read".
const answered = `{
  "id": "gen-dec-1790015143-AIaTutprXsJ5EwohRSjb",
  "model": "typesafe/jev-1.13-20260917",
  "provider": "TypeSafe",
  "answers": {
    "kind": {"type": "choice", "choice": "code", "confidence": 0.67,
             "probabilities": {"code": 0.78, "docs": 0.22, "investigation": 0, "design": 0}},
    "settled": {"type": "noul", "noul": 0.96},
    "reach": {"type": "score", "score": 1.99, "confidence": 0.99,
              "probabilities": {"0": 0, "1": 0, "2": 1},
              "legend": {"0": "one file", "1": "a few", "2": "many"}}
  },
  "usage": {"input_tokens": 476, "output_tokens": 70, "cost": 0.000019992}
}`

// asked is a set covering all three question shapes, so the request built
// for each and the answer read back for each are both exercised at once.
func asked() Questions {
	return Questions{
		"kind": {Type: KindChoice, Instructions: "What kind of work is this?", Criteria: map[string]string{
			"docs": "prose only", "code": "the program changes", "investigation": "read-only", "design": "the approach is not stated",
		}},
		"settled": {Type: KindNoul, Instructions: "Is every open point settled?"},
		"reach":   {Type: KindScore, Instructions: "How far does it reach?", Criteria: []string{"one file", "a few", "many"}},
	}
}

func serving(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return server
}

// stalling is a server that never answers. Its handler is released before
// the server is closed - a handler still running is one the server waits
// for, so a test that left it blocked would hang rather than fail.
func stalling(t *testing.T) *httptest.Server {
	t.Helper()
	released := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-released:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(released) })
	return server
}

func clientFor(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	t.Setenv(keyVariable, theKey)
	client, err := New(Endpoint{Provider: "TypeSafe", Model: "typesafe/jev-1.13", BaseURL: server.URL, APIKeyEnv: keyVariable}, server.Client())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return client
}

// The request has to be the one the service documents, field for field: a
// client that posts a chat completion to this address, or puts the key
// anywhere but the header, reaches nothing and reports it as the model
// being unavailable.
func TestJudgeBuildsTheRequestTheServiceDocuments(t *testing.T) {
	var method, path, auth, contentType string
	var body []byte
	server := serving(t, func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		auth, contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, answered)
	})
	if _, err := clientFor(t, server).Judge(t.Context(), ReceptionState{Request: "add a line"}, asked()); err != nil {
		t.Fatalf("judging: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if path != "/decisions" {
		t.Errorf("path = %q, want /decisions", path)
	}
	if auth != "Bearer "+theKey {
		t.Errorf("the key did not travel in the Authorization header")
	}
	if contentType != "application/json" {
		t.Errorf("content type = %q, want application/json", contentType)
	}
	var sent struct {
		Model string `json:"model"`
		State struct {
			Request string `json:"request"`
		} `json:"state"`
		Questions map[string]struct {
			Type         string          `json:"type"`
			Instructions string          `json:"instructions"`
			Criteria     json.RawMessage `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("the body is not the documented request: %v", err)
	}
	if sent.Model != "typesafe/jev-1.13" {
		t.Errorf("model = %q", sent.Model)
	}
	if sent.State.Request != "add a line" {
		t.Errorf("the state did not travel as JSON under its own key: %q", sent.State.Request)
	}
	if len(sent.Questions) != 3 {
		t.Fatalf("%d questions travelled, want 3", len(sent.Questions))
	}
	if sent.Questions["kind"].Type != "choice" || sent.Questions["kind"].Instructions == "" {
		t.Errorf("the choice question did not travel as one: %+v", sent.Questions["kind"])
	}
	// A choice's options travel as a map and a score's bands as a list. The
	// two are not interchangeable and sending the wrong one is refused by
	// the service, which the caller can only read as a failure.
	if !strings.HasPrefix(string(sent.Questions["kind"].Criteria), "{") {
		t.Errorf("a choice's criteria did not travel as a map: %s", sent.Questions["kind"].Criteria)
	}
	if !strings.HasPrefix(string(sent.Questions["reach"].Criteria), "[") {
		t.Errorf("a score's criteria did not travel as a list: %s", sent.Questions["reach"].Criteria)
	}
	if len(sent.Questions["settled"].Criteria) != 0 {
		t.Errorf("a noul travelled with criteria: %s", sent.Questions["settled"].Criteria)
	}
}

func TestJudgeReadsTheDocumentedAnswer(t *testing.T) {
	server := serving(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, answered) })
	answers, err := clientFor(t, server).Judge(t.Context(), ReceptionState{Request: "add a line"}, asked())
	if err != nil {
		t.Fatalf("judging: %v", err)
	}
	if answers.Provider != "TypeSafe" || answers.Model != "typesafe/jev-1.13-20260917" || answers.ID == "" {
		t.Errorf("the record of what answered was lost: %+v", answers)
	}
	option, confidence, ok := answers.Choice("kind")
	if !ok || option != "code" || confidence != 0.67 {
		t.Errorf("choice = %q, %v, %v; want code, 0.67, true", option, confidence, ok)
	}
	if got := answers.Answers["kind"].Probabilities["code"]; got != 0.78 {
		t.Errorf("the mass on the chosen option was lost: %v", got)
	}
	if answers.Answers["settled"].Noul == nil || *answers.Answers["settled"].Noul != 0.96 {
		t.Errorf("the noul was lost: %+v", answers.Answers["settled"])
	}
	if answers.Answers["reach"].Score == nil || *answers.Answers["reach"].Score != 1.99 {
		t.Errorf("the score was lost: %+v", answers.Answers["reach"])
	}
	if answers.Answers["reach"].Legend["2"] != "many" {
		t.Errorf("the score's legend was lost: %+v", answers.Answers["reach"].Legend)
	}
	if answers.Usage.InputTokens != 476 || answers.Usage.Cost != 0.000019992 {
		t.Errorf("the cost of the call was lost: %+v", answers.Usage)
	}
	// Nothing was asked under this id, so nothing may be read back under it
	// as though it had been.
	if _, _, ok := answers.Choice("nobody_asked"); ok {
		t.Errorf("an answer came back for a question nobody asked")
	}
}

// A model that has stopped answering must cost the run its bound and no
// more, and must not take the run down with it.
func TestADeadlineEndsTheCallWithoutPanicking(t *testing.T) {
	client := clientFor(t, stalling(t)).WithDeadline(50 * time.Millisecond)
	started := time.Now()
	answers, err := client.Judge(t.Context(), ReceptionState{Request: "add a line"}, asked())
	if err == nil {
		t.Fatalf("a call that was never answered returned %+v", answers)
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Errorf("the call waited %v, past its own bound", waited)
	}
	if len(answers.Answers) != 0 {
		t.Errorf("a failed call still returned answers: %+v", answers)
	}
}

func TestTheDefaultDeadlineIsTenSeconds(t *testing.T) {
	if DefaultDeadline != 10*time.Second {
		t.Errorf("DefaultDeadline = %v, want 10s", DefaultDeadline)
	}
}

// Every shape of unusable answer is an error, and none of them is a
// judgment. A caller that read one as a "no" would act on an opinion the
// model never gave.
func TestAnAnswerThatCannotBeTrustedIsAnError(t *testing.T) {
	for _, testCase := range []struct{ name, reply string }{
		{"not JSON at all", "<html>502 Bad Gateway</html>"},
		{"the answers are at the top level", `{"kind":{"type":"choice","choice":"code"}}`},
		{"a question is left unanswered", `{"answers":{"kind":{"type":"choice","choice":"code"},"settled":{"type":"noul","noul":0.4}}}`},
		{"a choice names an option nobody offered", `{"answers":{"kind":{"type":"choice","choice":"refactor"},"settled":{"type":"noul","noul":0.4},"reach":{"type":"score","score":1}}}`},
		{"a choice names nothing", `{"answers":{"kind":{"type":"choice"},"settled":{"type":"noul","noul":0.4},"reach":{"type":"score","score":1}}}`},
		{"a probability is not one", `{"answers":{"kind":{"type":"choice","choice":"code"},"settled":{"type":"noul","noul":4.2},"reach":{"type":"score","score":1}}}`},
		{"a confidence is not one", `{"answers":{"kind":{"type":"choice","choice":"code","confidence":8},"settled":{"type":"noul","noul":0.4},"reach":{"type":"score","score":1}}}`},
		{"a score is off its scale", `{"answers":{"kind":{"type":"choice","choice":"code"},"settled":{"type":"noul","noul":0.4},"reach":{"type":"score","score":9}}}`},
		{"nothing was answered", `{"answers":{}}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := serving(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, testCase.reply) })
			answers, err := clientFor(t, server).Judge(t.Context(), ReceptionState{Request: "add a line"}, asked())
			if err == nil {
				t.Fatalf("this was read as a judgment: %+v", answers)
			}
			if len(answers.Answers) != 0 {
				t.Errorf("a refused answer still carried decisions: %+v", answers)
			}
		})
	}
}

func TestARefusalCarriesTheServiceOwnWords(t *testing.T) {
	server := serving(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":"this key may not use this model"}`)
	})
	_, err := clientFor(t, server).Judge(t.Context(), ReceptionState{Request: "add a line"}, asked())
	if err == nil {
		t.Fatal("a refusal was read as a judgment")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "may not use this model") {
		t.Errorf("the refusal says nothing an operator can act on: %v", err)
	}
}

// The key is held in one header and must reach nothing else. Every failure
// path is walked, because an error string is what travels into a log and
// from there out of the machine.
func TestTheKeyNeverReachesAnError(t *testing.T) {
	unreachable := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	address := unreachable.URL
	transport := unreachable.Client()
	unreachable.Close()

	refusing := serving(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		// A service that echoed the key back must not be the way it escapes.
		io.WriteString(w, `{"error":"no such key: `+theKey+`"}`)
	})
	malformed := serving(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "not json") })
	slow := stalling(t)

	for _, testCase := range []struct {
		name  string
		build func(t *testing.T) *Client
		state any
	}{
		{"nothing is listening", func(t *testing.T) *Client {
			t.Setenv(keyVariable, theKey)
			client, err := New(Endpoint{Provider: "TypeSafe", Model: "m", BaseURL: address, APIKeyEnv: keyVariable}, transport)
			if err != nil {
				t.Fatal(err)
			}
			return client
		}, ReceptionState{Request: "x"}},
		{"the key is refused", func(t *testing.T) *Client { return clientFor(t, refusing) }, ReceptionState{Request: "x"}},
		{"the answer is unreadable", func(t *testing.T) *Client { return clientFor(t, malformed) }, ReceptionState{Request: "x"}},
		{"the call runs out of time", func(t *testing.T) *Client {
			return clientFor(t, slow).WithDeadline(50 * time.Millisecond)
		}, ReceptionState{Request: "x"}},
		{"the request cannot be built", func(t *testing.T) *Client { return clientFor(t, malformed) }, nil},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.build(t).Judge(t.Context(), testCase.state, asked())
			if err == nil {
				t.Fatal("expected a failure to inspect")
			}
			if strings.Contains(err.Error(), theKey) {
				t.Errorf("the key reached an error string")
			}
		})
	}
}

// A key that would split the header, or one pasted with the line it came
// on, is refused without the call being made and without saying what was
// read.
func TestAnUnusableKeyIsRefusedWithoutSayingWhatItWas(t *testing.T) {
	for _, testCase := range []struct{ name, value string }{
		{"empty", ""},
		{"trailing newline", theKey + "\n"},
		{"surrounded by space", "  " + theKey + "  "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var reached bool
			server := serving(t, func(w http.ResponseWriter, r *http.Request) { reached = true })
			t.Setenv(keyVariable, testCase.value)
			client, err := New(Endpoint{Provider: "TypeSafe", Model: "m", BaseURL: server.URL, APIKeyEnv: keyVariable}, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Judge(t.Context(), ReceptionState{Request: "x"}, asked()); err == nil {
				t.Fatal("an unusable key was used")
			} else if strings.Contains(err.Error(), strings.TrimSpace(testCase.value)) && testCase.value != "" {
				t.Errorf("the refusal quoted the key")
			}
			if reached {
				t.Error("the call was made with an unusable key")
			}
		})
	}
}

// An endpoint that cannot be called is refused where it is configured, so a
// mistake in a destination's file is a configuration error rather than a
// model that appears to be down.
func TestAnEndpointThatCannotBeCalledIsRefusedAtOnce(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		endpoint Endpoint
	}{
		{"no model", Endpoint{Provider: "p", BaseURL: DefaultBaseURL, APIKeyEnv: keyVariable}},
		{"no key variable", Endpoint{Provider: "p", Model: "m", BaseURL: DefaultBaseURL}},
		{"a key variable that is not one", Endpoint{Provider: "p", Model: "m", BaseURL: DefaultBaseURL, APIKeyEnv: "lower case"}},
		{"not encrypted", Endpoint{Provider: "p", Model: "m", BaseURL: "http://example.com/api/alpha", APIKeyEnv: keyVariable}},
		// An address carrying credentials would put them into the error a
		// transport failure reports.
		{"credentials in the address", Endpoint{Provider: "p", Model: "m", BaseURL: "https://user:" + theKey + "@example.com/api/alpha", APIKeyEnv: keyVariable}},
		{"credentials in the query", Endpoint{Provider: "p", Model: "m", BaseURL: "https://example.com/api/alpha?key=" + theKey, APIKeyEnv: keyVariable}},
		{"a trailing slash", Endpoint{Provider: "p", Model: "m", BaseURL: "https://example.com/api/alpha/", APIKeyEnv: keyVariable}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, err := New(testCase.endpoint, http.DefaultClient)
			if err == nil {
				t.Fatalf("an unusable endpoint was accepted: %+v", client.Endpoint())
			}
			if strings.Contains(err.Error(), theKey) {
				t.Errorf("the refusal carried the credential it refused")
			}
		})
	}
}

func TestAnEmptyAddressIsTheServiceOwn(t *testing.T) {
	client, err := New(Endpoint{Provider: "p", Model: "m", APIKeyEnv: keyVariable}, http.DefaultClient)
	if err != nil {
		t.Fatalf("an endpoint naming no address was refused: %v", err)
	}
	if got := client.Endpoint().BaseURL; got != DefaultBaseURL {
		t.Errorf("address = %q, want %q", got, DefaultBaseURL)
	}
}

// A call that cannot mean anything is refused before it is paid for, and
// the refusal names the question that is wrong.
func TestACallThatCannotMeanAnythingIsRefused(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		state     any
		questions Questions
		names     string
	}{
		{"nothing to judge", nil, asked(), ""},
		{"nothing asked", ReceptionState{Request: "x"}, Questions{}, ""},
		{"a question that says nothing", ReceptionState{Request: "x"}, Questions{"q": {Type: KindChoice, Criteria: map[string]string{"a": "one", "b": "two"}}}, "q"},
		{"a choice with one option", ReceptionState{Request: "x"}, Questions{"q": {Type: KindChoice, Instructions: "?", Criteria: map[string]string{"a": "one"}}}, "q"},
		{"a choice whose options are a list", ReceptionState{Request: "x"}, Questions{"q": {Type: KindChoice, Instructions: "?", Criteria: []string{"a", "b"}}}, "q"},
		{"a score whose bands are a map", ReceptionState{Request: "x"}, Questions{"q": {Type: KindScore, Instructions: "?", Criteria: map[string]string{"a": "one"}}}, "q"},
		{"a noul carrying criteria", ReceptionState{Request: "x"}, Questions{"q": {Type: KindNoul, Instructions: "?", Criteria: []string{"a"}}}, "q"},
		{"a kind that does not exist", ReceptionState{Request: "x"}, Questions{"q": {Type: "guess", Instructions: "?"}}, "q"},
		{"a state too large to send", ReceptionState{Request: strings.Repeat("x", MaxStateBytes+1)}, asked(), ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var reached bool
			server := serving(t, func(w http.ResponseWriter, r *http.Request) { reached = true })
			_, err := clientFor(t, server).Judge(t.Context(), testCase.state, testCase.questions)
			if err == nil {
				t.Fatal("a call that cannot mean anything was made")
			}
			if reached {
				t.Error("the call was paid for before it was checked")
			}
			if testCase.names != "" && !strings.Contains(err.Error(), testCase.names) {
				t.Errorf("the refusal does not name the question that is wrong: %v", err)
			}
		})
	}
}

func TestTooManyQuestionsIsRefused(t *testing.T) {
	questions := Questions{}
	for index := range MaxQuestions + 1 {
		questions[string(rune('a'+index%26))+strings.Repeat("x", index)] = Question{
			Type: KindNoul, Instructions: "?",
		}
	}
	server := serving(t, func(w http.ResponseWriter, r *http.Request) { t.Error("the call was made") })
	if _, err := clientFor(t, server).Judge(t.Context(), ReceptionState{Request: "x"}, questions); err == nil {
		t.Fatal("an unbounded question set was sent")
	}
}

func TestACancelledContextEndsTheCall(t *testing.T) {
	server := serving(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := clientFor(t, server).Judge(ctx, ReceptionState{Request: "x"}, asked()); err == nil {
		t.Fatal("a cancelled call still returned a judgment")
	}
}
