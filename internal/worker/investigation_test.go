package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// loopScriptAPI answers each turn from a script and keeps every request.
type loopScriptAPI struct {
	answers  []string
	requests []ChatRequest
}

func (f *loopScriptAPI) ChatCompletions(_ context.Context, _ ModelEndpoint, request ChatRequest) (*ChatResponse, error) {
	f.requests = append(f.requests, request)
	if len(f.answers) == 0 {
		return nil, errors.New("script exhausted")
	}
	answer := f.answers[0]
	f.answers = f.answers[1:]
	if strings.HasPrefix(answer, malformedUsageMarker) {
		output := chatOutput(strings.TrimPrefix(answer, malformedUsageMarker))
		output.Usage.TotalTokens = output.Usage.PromptTokens + output.Usage.CompletionTokens + 7
		return output, nil
	}
	return chatOutput(answer), nil
}

// malformedUsageMarker makes the scripted API return the answer with a usage
// block whose total does not add up, the shape the live gateway returned once.
const malformedUsageMarker = "\x00malformed\x00"

func investigationFixture(t *testing.T, maxProbes int) (InvestigationInput, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "page.tmpl"), []byte("<h1>Old label</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := probe.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := probe.OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: probe.Limits{MaxProbes: maxProbes, MaxTotalBytes: 1 << 20, ExcerptBytes: 1 << 10}}
	identity := investigate.Identity{DeliveryID: "delivery-1", InputSHA256: strings.Repeat("1", 64), ConfigSHA256: strings.Repeat("2", 64), ToolSHA: "tool", BaseSHA: strings.Repeat("a", 40)}
	input := InvestigationInput{
		Identity: identity, Round: 1, Mode: ModeInvestigation,
		Request: TicketRequest{IssueKey: "T-1", Summary: "Rename the label", Request: "Change Old label to New label", TargetFiles: []string{"web/page.tmpl"}},
		Session: session, MeasurementsPath: path,
		Bounds: investigate.Bounds{AllowedFilePrefixes: []string{"web/"}, MaxFiles: 4, Catalog: catalog, RepoRoot: root},
	}
	return input, path
}

const reportAnswer = `{"report":{"questions":["Where is the label?"],"findings":[{"claim":"The label is in web/page.tmpl","evidence":["m-0002"],"confidence":"measured"}],"unknowns":[],"next":"Replace it."}}`
const designAnswer = `{"design":{"cause":"The label is hard-coded","cause_evidence":["m-0002"],"approach":"Replace the label","alternatives":["Add a translation key"],"files":[{"path":"web/page.tmpl","changes":["replace Old label with New label"]}],"verification":{"form":"wording","path":"/page","expected_text":"New label","absent_text":"Old label"},"blast_radius":["the page header"],"not_doing":[]}}`

func TestInvestigateSealsReportAndDesignFromOneConversation(t *testing.T) {
	input, path := investigationFixture(t, 10)
	input.Mode = ModeDesign
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.list"}}`,
		`{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`,
		reportAnswer,
		designAnswer,
	}}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{ID: "designer", Vendor: "v", Model: "m", BaseURL: "https://gateway.example.invalid", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate: %v (%s)", err, result.Incomplete)
	}
	if result.Turns != 4 || result.Investigation.MeasurementsCount != 2 || result.Investigation.ProbesUsed != 2 || result.Design == nil {
		t.Fatalf("result: turns %d investigation %+v design %v", result.Turns, result.Investigation, result.Design)
	}
	if err := result.Investigation.Validate(input.Identity, path); err != nil {
		t.Errorf("sealed report: %v", err)
	}
	if err := result.Design.Validate(input.Identity, result.Investigation, input.Bounds); err != nil {
		t.Errorf("sealed design: %v", err)
	}
	// The model saw the recorded outcome and an excerpt, never the raw file directly.
	third := api.requests[2].Messages
	if !strings.Contains(third[len(third)-1].Content, `"excerpt":"<h1>Old label</h1>\n"`) && !strings.Contains(third[len(third)-1].Content, "Old label") {
		t.Errorf("model was not shown the excerpt: %s", third[len(third)-1].Content)
	}
	if !strings.Contains(api.requests[3].Messages[len(api.requests[3].Messages)-1].Content, `"sealed":"investigation"`) {
		t.Error("model was not told the investigation was sealed")
	}
	if !strings.Contains(api.requests[0].Messages[1].Content, `"catalogue"`) || !strings.Contains(api.requests[0].Messages[0].Content, "exactly one JSON object") {
		t.Error("task prompt lacks the catalogue or the contract")
	}
}

func TestInvestigateRefusesProbesAfterTheReportIsSealed(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	input.Mode = ModeDesign
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`,
		`{"report":{"questions":["Where is the label?"],"findings":[{"claim":"The label is in web/page.tmpl","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Replace it."}}`,
		`{"probe":{"probe":"repo.list"}}`, // after the seal: objected, not executed
		`{"design":{"cause":"The label is hard-coded","cause_evidence":["m-0001"],"approach":"Replace the label","alternatives":["Add a translation key"],"files":[{"path":"web/page.tmpl","changes":["replace Old label with New label"]}],"verification":{"form":"wording","path":"/page","expected_text":"New label","absent_text":"Old label"},"blast_radius":["the page header"],"not_doing":[]}}`,
	}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate: %v (%s)", err, result.Incomplete)
	}
	if input.Session.Used != 1 || result.Investigation.ProbesUsed != 1 || result.Investigation.MeasurementsCount != 1 || result.Design == nil {
		t.Errorf("a probe ran after the seal: used %d sealed %d", input.Session.Used, result.Investigation.ProbesUsed)
	}
	last := api.requests[3].Messages[len(api.requests[3].Messages)-1].Content
	if !strings.Contains(last, "measurements are closed") && !strings.Contains(last, "no more measurements") {
		t.Errorf("model was not told the measurements are closed: %s", last)
	}
}

func TestInvestigateObjectsToUnsupportedReportsAndBudgetOverruns(t *testing.T) {
	input, path := investigationFixture(t, 1)
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`,
		`{"probe":{"probe":"repo.list"}}`, // over budget: told to answer now
		`{"report":{"questions":["q"],"findings":[{"claim":"c","evidence":["m-0007"],"confidence":"measured"}],"unknowns":[],"next":"n"}}`, // cites nothing sealed
		`not json`,
		`{"report":{"questions":["q"],"findings":[{"claim":"The label is in the template","evidence":["m-0001"],"confidence":"measured"}],"unknowns":["caching"],"next":"Replace it."}}`,
	}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate: %v (%s)", err, result.Incomplete)
	}
	if result.Turns != 5 || result.Investigation.ProbesUsed != 1 || result.Investigation.MeasurementsCount != 1 {
		t.Errorf("result: %+v", result)
	}
	if err := result.Investigation.Validate(input.Identity, path); err != nil {
		t.Errorf("sealed report: %v", err)
	}
	messages := api.requests[4].Messages
	joined := ""
	for _, message := range messages {
		joined += message.Content + "\n"
	}
	for _, want := range []string{`"budget":"exhausted"`, "not among the sealed measurements", `"rejected"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("conversation lacks %q", want)
		}
	}
	// Three refused answers in a row end the round honestly.
	input2, _ := investigationFixture(t, 5)
	api2 := &loopScriptAPI{answers: []string{`nope`, `{"probe":{},"report":{}}`, `{"design":{}}`}}
	invoker2, _ := NewModelInvoker(api2)
	result2, err := invoker2.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input2, time.Now())
	if !errors.Is(err, ErrInvestigationIncomplete) || !strings.Contains(result2.Incomplete, "contract") {
		t.Errorf("three refusals: %v %q", err, result2.Incomplete)
	}
	// A spent budget followed by another probe request ends the round honestly.
	input3, _ := investigationFixture(t, 1)
	api3 := &loopScriptAPI{answers: []string{`{"probe":{"probe":"repo.list"}}`, `{"probe":{"probe":"repo.list"}}`, `{"probe":{"probe":"repo.list"}}`}}
	invoker3, _ := NewModelInvoker(api3)
	result3, err := invoker3.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input3, time.Now())
	if !errors.Is(err, ErrInvestigationIncomplete) || !strings.Contains(result3.Incomplete, "budget") {
		t.Errorf("budget overrun: %v %q", err, result3.Incomplete)
	}
}

func TestInvestigationBudgetEndsHonestly(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	api := &loopScriptAPI{answers: []string{`{"probe":{"probe":"repo.list"}}`}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(ctx, ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if !errors.Is(err, ErrInvestigationIncomplete) || !strings.Contains(result.Incomplete, "wall") {
		t.Errorf("cancelled context: %v %q", err, result.Incomplete)
	}
}

func TestInvestigationWithdrawsOldExcerptsOverTheBudget(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	input.ExcerptBudget = 40
	big := strings.Repeat("x", 30)
	if err := os.WriteFile(filepath.Join(input.Bounds.RepoRoot, "web", "a.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input.Bounds.RepoRoot, "web", "b.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/a.txt"}}}`,
		`{"probe":{"probe":"repo.read","args":{"path":"web/b.txt"}}}`,
		`{"report":{"questions":["q"],"findings":[{"claim":"both files carry the marker","evidence":["m-0001","m-0002"],"confidence":"measured"}],"unknowns":[],"next":"n"}}`,
	}}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); err != nil {
		t.Fatal(err)
	}
	messages := api.requests[2].Messages
	var withdrawn struct {
		ID        string `json:"measurement_id"`
		Withdrawn bool   `json:"excerpt_withdrawn"`
		Head      string `json:"head"`
	}
	if err := json.Unmarshal([]byte(messages[3].Content), &withdrawn); err != nil || !withdrawn.Withdrawn || withdrawn.ID != "m-0001" || !strings.HasPrefix(withdrawn.Head, "xxx") {
		t.Errorf("first excerpt not withdrawn: %s (%v)", messages[3].Content, err)
	}
	if !strings.Contains(messages[5].Content, `"excerpt":"xxxxxxxxxx`) {
		t.Errorf("latest excerpt withdrawn too: %s", messages[5].Content)
	}
}

// The model is told how to choose among a probe's hosts: the catalogue entry
// of an http probe with several hosts carries host_argument, one with a
// single host does not (live 2026-09-05: without it every timing went to
// the first host).
func TestInvestigationTaskPromptNamesTheHostArgumentForMultiHostProbes(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	multi, err := probe.NewCatalog([]probe.Spec{{ID: "http.timing", Kind: probe.KindHTTP, Hosts: []string{"console.example.invalid", "api.example.invalid"},
		Methods: []string{"GET"}, Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z]{0,20}`}}})
	if err != nil {
		t.Fatal(err)
	}
	input.Session.Catalog = multi
	if prompt := investigationTaskPrompt(input); !strings.Contains(prompt, `"host_argument":"add \"host\" to args, one of hosts; omitted = hosts[0]"`) {
		t.Errorf("multi-host entry lacks host_argument: %s", prompt)
	}
	single, err := probe.NewCatalog([]probe.Spec{{ID: "http.timing", Kind: probe.KindHTTP, Hosts: []string{"console.example.invalid"},
		Methods: []string{"GET"}, Returns: []string{"status", "time_total", "bytes"}, Args: map[string]string{"path": `/[a-z]{0,20}`}}})
	if err != nil {
		t.Fatal(err)
	}
	input.Session.Catalog = single
	if prompt := investigationTaskPrompt(input); strings.Contains(prompt, "host_argument") {
		t.Errorf("single-host entry carries host_argument: %s", prompt)
	}
}

// One out-of-shape response does not end the round: the same turn is asked
// again and the round seals; two in a row are the transport's failure.
func TestInvestigateAsksAgainOnceAfterAMalformedResponse(t *testing.T) {
	input, path := investigationFixture(t, 10)
	report := `{"report":{"questions":["Where is the label?"],"findings":[{"claim":"The label is in web/page.tmpl","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Replace it."}}`
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`,
		malformedUsageMarker + report,
		report,
	}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate: %v (%s)", err, result.Incomplete)
	}
	if len(api.requests) != 3 || result.Turns != 2 || result.Investigation.MeasurementsCount != 1 {
		t.Fatalf("requests %d turns %d measurements %d; want the malformed turn asked again", len(api.requests), result.Turns, result.Investigation.MeasurementsCount)
	}
	if err := result.Investigation.Validate(input.Identity, path); err != nil {
		t.Errorf("sealed report: %v", err)
	}

	input2, _ := investigationFixture(t, 10)
	api2 := &loopScriptAPI{answers: []string{
		malformedUsageMarker + `{"probe":{"probe":"repo.list"}}`,
		malformedUsageMarker + `{"probe":{"probe":"repo.list"}}`,
		`{"probe":{"probe":"repo.list"}}`,
	}}
	invoker2, _ := NewModelInvoker(api2)
	if _, err := invoker2.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input2, time.Now()); !errors.Is(err, ErrModelResponseMetadata) {
		t.Fatalf("two malformed responses in a row: err = %v, want the metadata error", err)
	}
	if len(api2.requests) != 2 {
		t.Fatalf("requests after two malformed responses = %d, want 2", len(api2.requests))
	}
}
