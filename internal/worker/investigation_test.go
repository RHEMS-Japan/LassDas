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
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// loopScriptAPI answers each turn from a script and keeps every request.
// The tests exercise the re-ask path directly; the pause before asking again
// is for the live gateway, not for them.
func init() { malformedTurnDelay = 0 }

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
	switch {
	case strings.HasPrefix(answer, malformedUsageMarker):
		output := chatOutput(strings.TrimPrefix(answer, malformedUsageMarker))
		output.Usage.TotalTokens = output.Usage.PromptTokens + output.Usage.CompletionTokens + 7
		return output, nil
	case strings.HasPrefix(answer, noUsageMarker):
		output := chatOutput(strings.TrimPrefix(answer, noUsageMarker))
		output.Usage = nil
		return output, nil
	case answer == emptyContentMarker:
		return chatOutput(""), nil
	case strings.HasPrefix(answer, contentFilterMarker):
		output := chatOutput(strings.TrimPrefix(answer, contentFilterMarker))
		output.Choices[0].FinishReason = ChatFinishContentFilter
		return output, nil
	case strings.HasPrefix(answer, lengthMarker):
		output := chatOutput(strings.TrimPrefix(answer, lengthMarker))
		output.Choices[0].FinishReason = "length"
		return output, nil
	case strings.HasPrefix(answer, providerErrorMarker):
		output := chatOutput(strings.TrimPrefix(answer, providerErrorMarker))
		output.Choices[0].FinishReason = ChatFinishError
		return output, nil
	case strings.HasPrefix(answer, bareErrorMarker):
		// The provider's error with nothing else: no usage, empty content.
		output := chatOutput("")
		output.Choices[0].FinishReason = ChatFinishError
		output.Usage = nil
		return output, nil
	case answer == topLevelErrorMarker:
		return &ChatResponse{ID: "gen-topLevelError000000000", Error: &ChatResponseError{Code: 502}}, nil
	}
	return chatOutput(answer), nil
}

// The markers make the scripted API return an answer out of shape: a usage
// block whose total does not add up (the shape the live gateway returned
// once), no usage block, or an empty assistant message.
const (
	malformedUsageMarker = "\x00malformed\x00"
	noUsageMarker        = "\x00nousage\x00"
	emptyContentMarker   = "\x00empty\x00"
	contentFilterMarker  = "\x00filtered\x00"
	lengthMarker         = "\x00length\x00"
	providerErrorMarker  = "\x00error\x00"
	bareErrorMarker      = "\x00bare-error\x00"
	topLevelErrorMarker  = "\x00top-level-error\x00"
)

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
// again and the round seals with its measurements; two in a row are the
// transport's failure. The count restarts after every turn that came back
// in shape, and every shape refusal — usage that does not add up, no usage,
// an empty message — is asked again the same way.
func TestInvestigateAsksAgainOnceAfterAMalformedResponse(t *testing.T) {
	probeRead := `{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`
	probeList := `{"probe":{"probe":"repo.list"}}`
	report := `{"report":{"questions":["Where is the label?"],"findings":[{"claim":"The label is in web/page.tmpl","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Replace it."}}`

	// One malformed response, then the same turn answered in shape.
	input, path := investigationFixture(t, 10)
	api := &loopScriptAPI{answers: []string{probeRead, malformedUsageMarker + report, report}}
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

	// Malformed, in shape, malformed, in shape: the count restarts after
	// every turn that came back in shape, so the round still seals.
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{malformedUsageMarker + probeRead, probeRead, noUsageMarker + probeList, probeList, emptyContentMarker, report}}
	invoker, _ = NewModelInvoker(api)
	if result, err = invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); err != nil {
		t.Fatalf("alternating shapes: %v (%s)", err, result.Incomplete)
	}
	if len(api.requests) != 6 || result.Turns != 3 || result.Investigation.MeasurementsCount != 2 {
		t.Fatalf("alternating shapes: requests %d turns %d measurements %d", len(api.requests), result.Turns, result.Investigation.MeasurementsCount)
	}

	// Two in a row: the failure travels after exactly two requests.
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{malformedUsageMarker + probeList, malformedUsageMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); !errors.Is(err, errModelResponseMetadata) {
		t.Fatalf("two malformed responses in a row: err = %v, want the metadata error", err)
	}
	if len(api.requests) != 2 {
		t.Fatalf("requests after two malformed responses = %d, want 2", len(api.requests))
	}
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{emptyContentMarker, emptyContentMarker, probeList}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); !errors.Is(err, errModelResponseContent) || len(api.requests) != 2 {
		t.Fatalf("two empty messages in a row: err = %v after %d requests, want the content error after 2", err, len(api.requests))
	}
}

// A provider's refusal of one turn (finish_reason=content_filter) is asked
// again once like an out-of-shape response; two in a row travel with the
// refusal named. A length cutoff is asked again once with a wider allowance.
func TestInvestigateAsksAgainOnceAfterAContentFilterVerdict(t *testing.T) {
	probeList := `{"probe":{"probe":"repo.list"}}`
	report := `{"report":{"questions":["What is there?"],"findings":[{"claim":"The listing was taken","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Nothing."}}`
	input, _ := investigationFixture(t, 10)
	api := &loopScriptAPI{answers: []string{probeList, contentFilterMarker + report, report}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate after one refused turn: %v (%s)", err, result.Incomplete)
	}
	if len(api.requests) != 3 || result.Turns != 2 || result.Investigation.MeasurementsCount != 1 {
		t.Fatalf("requests %d turns %d measurements %d; want the refused turn asked again", len(api.requests), result.Turns, result.Investigation.MeasurementsCount)
	}
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{contentFilterMarker + probeList, contentFilterMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); !errors.Is(err, errModelResponseRefused) || len(api.requests) != 2 || !strings.Contains(err.Error(), "content_filter") {
		t.Fatalf("two refused turns in a row: err = %v after %d requests, want the refusal named after 2", err, len(api.requests))
	}

	// A length cutoff is asked again once with a wider allowance: the second
	// answer goes through and the round continues (here until the script
	// runs out). Two cutoffs in a row travel named after exactly two requests.
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{lengthMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); errors.Is(err, errModelResponseTruncated) || len(api.requests) < 2 || api.requests[1].MaxTokens != 8192 {
		t.Fatalf("a length cutoff: err = %v after %d requests, want the turn asked again with 8192 tokens of room", err, len(api.requests))
	}
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{lengthMarker + probeList, lengthMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); !errors.Is(err, errModelResponseTruncated) || len(api.requests) != 2 || !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("two length cutoffs: err = %v after %d requests, want the cutoff named after 2", err, len(api.requests))
	}
}

// An output longer than the excerpt is read on: the role asks for the next
// window at the excerpt's end, then at each next_offset, and the windows
// together with the excerpt are the stored output; a bad offset is
// objected to, and the read budget ends the paging honestly.
func TestInvestigateReadsBeyondTheExcerpt(t *testing.T) {
	input, path := investigationFixture(t, 10)
	long := strings.Repeat("line of the event list\n", 200) // 4600 bytes, excerpt is 1 KiB
	if err := os.WriteFile(filepath.Join(input.Bounds.RepoRoot, "web", "events.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	input.Session.Limits.MaxReads = 3
	report := `{"report":{"questions":["How long is the list?"],"findings":[{"claim":"The list has 200 lines","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Nothing."}}`
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/events.txt"}}}`,
		`{"read":{"id":"m-0001","offset":1024}}`,
		`{"read":{"id":"m-0001","offset":99999}}`, // outside the record: objected, not counted as a window
		`{"read":{"id":"m-0001","offset":2048}}`,
		`{"read":{"id":"m-0001","offset":3072}}`, // the fourth request: over the read budget of 3
		report,
	}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate() error = %v (%s)", err, result.Incomplete)
	}
	// Two windows shown; the refused offset counted too; the request over
	// the budget was not (nothing was looked up for it).
	if result.Reads != 2 || input.Session.Reads != 3 {
		t.Fatalf("reads = %d shown, %d counted; want 2 windows shown and the refused request counted", result.Reads, input.Session.Reads)
	}
	if len(api.requests) != 6 {
		t.Fatalf("requests = %d, want one per answer", len(api.requests))
	}
	// The window messages carry the stored bytes from the offset on.
	messages := api.requests[2].Messages
	last := messages[len(messages)-1].Content
	if !strings.Contains(last, `"offset":1024`) || !strings.Contains(last, `"next_offset":2048`) || !strings.Contains(last, `"remaining":2552`) || !strings.Contains(last, `"stored_bytes":4600`) {
		t.Fatalf("the first window was not shown as recorded: %s", last[:200])
	}
	objection := api.requests[3].Messages
	if !strings.Contains(objection[len(objection)-1].Content, "outside the stored output") {
		t.Fatalf("a bad offset was not objected to: %s", objection[len(objection)-1].Content[:200])
	}
	exhausted := api.requests[5].Messages
	if !strings.Contains(exhausted[len(exhausted)-1].Content, "read budget is spent") {
		t.Fatalf("the read budget was not announced: %s", exhausted[len(exhausted)-1].Content[:200])
	}
	measurements, err := probe.ReadPrefix(path, 1)
	if err != nil || len(measurements) != 1 || measurements[0].OutputBytes != 4600 {
		t.Fatalf("the read did not stay off the record: %+v, %v", measurements, err)
	}
}

// A read is one of the four shapes; combined with another, or malformed,
// it is refused like any other out-of-contract answer.
func TestDecodeTurnAnswerAcceptsARead(t *testing.T) {
	if answer, err := decodeTurnAnswer([]byte(`{"read":{"id":"m-0002","offset":0}}`), ModeInvestigation); err != nil || answer.Read == nil || answer.Read.ID != "m-0002" {
		t.Fatalf("a read was not accepted: %+v, %v", answer, err)
	}
	if _, err := decodeTurnAnswer([]byte(`{"read":{"id":"m-0002","offset":0},"probe":{"probe":"repo.list"}}`), ModeInvestigation); err == nil {
		t.Fatal("a read combined with a probe was accepted")
	}
	if answer, err := decodeTurnAnswer([]byte(`{"read":{"id":"m-0002","offset":0}}`), ModeDesign); err != nil || answer.Read == nil {
		t.Fatalf("a read after the seal was refused: %v", err)
	}
}

// Windows count toward the conversation's excerpt budget and are withdrawn
// like excerpts, named by the measurement id and the window's offset so
// the id alone stays citable.
func TestInvestigationWithdrawsOldWindowsOverTheBudget(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	input.ExcerptBudget = 1200 // one excerpt (1 KiB) plus a little
	if err := os.WriteFile(filepath.Join(input.Bounds.RepoRoot, "web", "events.txt"), []byte(strings.Repeat("y", 3000)), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.read","args":{"path":"web/events.txt"}}}`,
		`{"read":{"id":"m-0001","offset":1024}}`,
		`{"read":{"id":"m-0001","offset":2048}}`,
		`{"report":{"questions":["q"],"findings":[{"claim":"3000 bytes","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"n"}}`,
	}}
	invoker, _ := NewModelInvoker(api)
	if _, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); err != nil {
		t.Fatal(err)
	}
	messages := api.requests[3].Messages
	var withdrawn struct {
		ID        string `json:"measurement_id"`
		Offset    int    `json:"window_offset"`
		Withdrawn bool   `json:"window_withdrawn"`
		Head      string `json:"head"`
	}
	// The excerpt (message 3) and the first window (message 5) were withdrawn
	// once the second window arrived; the latest window stays.
	if !strings.Contains(messages[3].Content, `"excerpt_withdrawn":true`) {
		t.Errorf("the excerpt was not withdrawn: %s", messages[3].Content[:120])
	}
	if err := json.Unmarshal([]byte(messages[5].Content), &withdrawn); err != nil || !withdrawn.Withdrawn || withdrawn.ID != "m-0001" || withdrawn.Offset != 1024 || !strings.HasPrefix(withdrawn.Head, "yyyy") {
		t.Errorf("the first window was not withdrawn by id and offset: %s (%v)", messages[5].Content[:160], err)
	}
	if !strings.Contains(messages[7].Content, `"offset":2048`) || !strings.Contains(messages[7].Content, `"text":"yyyy`) {
		t.Errorf("the latest window was withdrawn too: %s", messages[7].Content[:120])
	}
}

// A report the contract keeps refusing ends the round with the last refused
// answer and the objection in the result, and the objection names the
// line and the rule.
func TestInvestigateKeepsTheLastRefusedAnswer(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	long := `{"report":{"questions":["q"],"findings":[{"claim":"` + strings.Repeat("x", 601) + `","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"n"}}`
	api := &loopScriptAPI{answers: []string{`{"probe":{"probe":"repo.list"}}`, long, long, long}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if !errors.Is(err, ErrInvestigationIncomplete) || !strings.Contains(result.Incomplete, "claim is 601 bytes (limit 600)") {
		t.Fatalf("err = %v, incomplete = %q; want the refusal to name the rule", err, result.Incomplete)
	}
	if result.LastRefusedAnswer != long || !strings.Contains(result.LastRefusedObjection, "finding 1: claim is 601 bytes (limit 600)") {
		t.Fatalf("last refused answer/objection not kept: %d bytes / %q", len(result.LastRefusedAnswer), result.LastRefusedObjection)
	}
	messages := api.requests[2].Messages
	if !strings.Contains(messages[len(messages)-1].Content, "finding 1: claim is 601 bytes (limit 600)") {
		t.Fatalf("the role was not told the rule: %s", messages[len(messages)-1].Content[:200])
	}
	bounded := boundedAnswer(strings.Repeat("あ", 4000))
	if len(bounded) > maxKeptAnswerBytes+len("…") || !strings.HasSuffix(bounded, "…") || !utf8.ValidString(bounded) {
		t.Fatalf("a kept answer must be bounded on a character boundary: %d bytes", len(bounded))
	}
	// A refusal followed by an accepted answer is forgotten: a round that
	// then ends on the probe budget records no refused answer.
	input, _ = investigationFixture(t, 1)
	api = &loopScriptAPI{answers: []string{long, `{"probe":{"probe":"repo.list"}}`, `{"probe":{"probe":"repo.list"}}`, `{"probe":{"probe":"repo.list"}}`}}
	invoker, _ = NewModelInvoker(api)
	result, err = invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if !errors.Is(err, ErrInvestigationIncomplete) || !strings.Contains(result.Incomplete, "probe budget") || result.LastRefusedAnswer != "" || result.LastRefusedObjection != "" {
		t.Fatalf("a budget ending kept a stale refusal: %q / %q (%v)", result.Incomplete, result.LastRefusedObjection, err)
	}
}

// The design instruction states what absent_text is and when to leave it
// empty, the rule the kernel's wording check refuses on (a live round
// spent its attempts on that guess); the investigation-only instruction
// carries no design section.
func TestInvestigationSystemPromptStatesTheAbsentTextRule(t *testing.T) {
	for _, revise := range []bool{false, true} {
		design := investigationSystemPrompt(ModeDesign, revise)
		if !strings.Contains(design, investigate.VerificationRules) {
			t.Errorf("design instruction lacks the shared verification contract (revise=%v)", revise)
		}
	}
	if strings.Contains(investigationSystemPrompt(ModeInvestigation, false), investigate.VerificationRules) {
		t.Error("the investigation-only instruction talks about a design it never asks for")
	}
}

// The refusal must tell the designer how to recover from the unsupported
// metric a reviewer suggested. The supported wording form must seal for an
// addition to an existing file without promising to remove existing text.
func TestInvestigateCorrectsUnknownMetricToAdditiveWording(t *testing.T) {
	input, _ := investigationFixture(t, 10)
	input.Mode = ModeDesign
	input.Request.Request = "Append New label and preserve the existing text"
	var answer struct {
		Design investigate.ModelDesignOutput `json:"design"`
	}
	if err := json.Unmarshal([]byte(designAnswer), &answer); err != nil {
		t.Fatal(err)
	}
	answer.Design.Approach = "Append the requested label"
	answer.Design.Files[0].Changes = []string{"append New label, preserving existing text"}
	answer.Design.Verification.AbsentText = ""
	corrected, _ := json.Marshal(answer)
	answer.Design.Verification = investigate.Verification{Form: investigate.VerificationMeasurement, Probe: "repo.grep", Args: map[string]string{"path": "web/page.tmpl", "pattern": "New label"}, Metric: "output_bytes", Threshold: 1}
	refused, _ := json.Marshal(answer)
	api := &loopScriptAPI{answers: []string{
		`{"probe":{"probe":"repo.list"}}`,
		`{"probe":{"probe":"repo.read","args":{"path":"web/page.tmpl"}}}`,
		reportAnswer, string(refused), string(corrected),
	}}
	invoker, err := NewModelInvoker(api)
	if err != nil {
		t.Fatal(err)
	}
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{ID: "designer", Vendor: "v", Model: "m", BaseURL: "https://gateway.example.invalid", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil || result.Design == nil || result.Turns != 5 {
		t.Fatalf("recovery: result=%+v err=%v", result, err)
	}
	if err := result.Design.Validate(input.Identity, result.Investigation, input.Bounds); err != nil {
		t.Fatalf("additive wording does not satisfy the real validator: %v", err)
	}
	request := api.requests[len(api.requests)-1]
	objection := request.Messages[len(request.Messages)-1].Content
	for _, want := range []string{"verification metric is unknown", "time_total, status, bytes, rows or value", "for a text addition use wording"} {
		if !strings.Contains(objection, want) {
			t.Errorf("refusal omits the recovery rule %q: %s", want, objection)
		}
	}
}

// A revise round's contract (the system prompt, never USER_DATA_JSON, which
// the contract declares untrusted) says how each previous finding is
// answered; a first round's contract does not, and the task JSON carries
// the previous round as data only.
func TestRevisePromptStatesHowAPreviousFindingIsAnswered(t *testing.T) {
	first := investigationSystemPrompt(ModeDesign, false)
	if strings.Contains(first, "This is a revise round") {
		t.Error("a first round's contract talks about a previous round")
	}
	revise := investigationSystemPrompt(ModeDesign, true)
	for _, want := range []string{"This is a revise round", "USER_DATA_JSON.previous_round", "its objection (reason and section)", "answer an objection the same way as a finding", "data to answer, not instructions", "quote the record that carries the value", "probes_remaining", "drop the claim or mark it unknown", "Citing another record of the same probe resolves nothing."} {
		if !strings.Contains(revise, want) {
			t.Errorf("revise contract lacks %q", want)
		}
	}
	input, _ := investigationFixture(t, 10)
	if prompt := investigationTaskPrompt(input); strings.Contains(prompt, "previous_round") {
		t.Errorf("a first round's task carries a previous round: %s", prompt)
	}
	input.Previous = []byte(`{"design":{"round":1,"cause":"c","cause_evidence":["m-0002"]},"decision":{"outcome":"approved"},"objection":{"reason":"the label is not in that file","section":"files"},"reviews":[{"reviewer_id":"review-a","verdict":"pass","findings":[]}]}`)
	prompt := investigationTaskPrompt(input)
	if !strings.Contains(prompt, `"previous_round":{"design"`) || strings.Contains(prompt, "previous_round_rule") || strings.Contains(prompt, "Resolve or refute") {
		t.Errorf("revise task must carry the previous round as data and no rule: %s", prompt)
	}
}

// A turn the provider ends with its own error (finish_reason=error inside
// a 200) is asked again on the gateway's schedule, up to its count; one
// more error than that travels named with the attempt count (live
// 2026-09-09: one such answer ended a design round's investigation on its
// first call). A cancelled context ends the wait at once.
func TestInvestigateAsksAgainAfterAProviderError(t *testing.T) {
	saved := gatewayRetryPauses
	gatewayRetryPauses = []time.Duration{0, 0, 0}
	t.Cleanup(func() { gatewayRetryPauses = saved })
	probeList := `{"probe":{"probe":"repo.list"}}`
	report := `{"report":{"questions":["What is there?"],"findings":[{"claim":"The listing was taken","evidence":["m-0001"],"confidence":"measured"}],"unknowns":[],"next":"Nothing."}}`
	input, _ := investigationFixture(t, 10)
	api := &loopScriptAPI{answers: []string{providerErrorMarker + probeList, providerErrorMarker + probeList, providerErrorMarker + probeList, probeList, report}}
	invoker, _ := NewModelInvoker(api)
	result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if err != nil {
		t.Fatalf("Investigate after three provider errors: %v (%s)", err, result.Incomplete)
	}
	if len(api.requests) != 5 || result.Turns != 2 || result.Investigation.MeasurementsCount != 1 {
		t.Fatalf("requests %d turns %d measurements %d; want the errored turn asked again three times", len(api.requests), result.Turns, result.Investigation.MeasurementsCount)
	}
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{providerErrorMarker + probeList, providerErrorMarker + probeList, providerErrorMarker + probeList, providerErrorMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	_, err = invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
	if !errors.Is(err, errModelResponseUpstream) || len(api.requests) != 4 || !strings.Contains(err.Error(), "finish_reason=error") || !strings.Contains(err.Error(), "after 4 provider errors") {
		t.Fatalf("four provider errors in a row: err = %v after %d requests, want the error named after 4", err, len(api.requests))
	}
	gatewayRetryPauses = []time.Duration{10 * time.Second}
	input, _ = investigationFixture(t, 10)
	api = &loopScriptAPI{answers: []string{providerErrorMarker + probeList, probeList}}
	invoker, _ = NewModelInvoker(api)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	began := time.Now()
	if _, err := invoker.Investigate(ctx, ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now()); err == nil || time.Since(began) > 5*time.Second || len(api.requests) != 1 {
		t.Fatalf("a cancelled wait: err = %v after %s and %d requests, want the wait ended by the context", err, time.Since(began), len(api.requests))
	}
	// The provider's error may arrive with no usage and no content, or as a
	// top-level error with no choices: both are the same transient, judged
	// before the usage and content checks, and asked again the same way.
	gatewayRetryPauses = []time.Duration{0, 0, 0}
	for name, marker := range map[string]string{"bare error": bareErrorMarker + probeList, "top-level error": topLevelErrorMarker} {
		input, _ = investigationFixture(t, 10)
		api = &loopScriptAPI{answers: []string{marker, marker, probeList, report}}
		invoker, _ = NewModelInvoker(api)
		result, err := invoker.Investigate(context.Background(), ModelEndpoint{Model: "m", MaxOutputTokens: 4096}, input, time.Now())
		if err != nil || len(api.requests) != 4 || result.Investigation.MeasurementsCount != 1 {
			t.Fatalf("%s: err = %v after %d requests, want two errors asked again and the round completed", name, err, len(api.requests))
		}
	}
}

// A revise round is told which records the run holds so far and that it
// can read any of them from offset 0; the earlier round's conversation is
// gone, and without the index the role could only cite ids it never saw
// or measure again (live: two rounds were spent swapping ids).
func TestReviseRoundListsEarlierRecordsAndMayReadFromTheStart(t *testing.T) {
	input, measurementsPath := investigationFixture(t, 10)
	// Record two measurements the way the earlier round did.
	for _, request := range []probe.Request{{Probe: "repo.list"}, {Probe: "repo.list"}} {
		if _, err := input.Session.Run(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	input.MeasurementsPath = measurementsPath
	if prompt := investigationTaskPrompt(input); strings.Contains(prompt, "earlier_records") {
		t.Errorf("a first round lists earlier records: %s", prompt)
	}
	input.Previous = []byte(`{"design":{"round":1},"decision":{"outcome":"revise"},"reviews":[]}`)
	prompt := investigationTaskPrompt(input)
	for _, want := range []string{`"earlier_records":[{"id":"m-0001","probe":"repo.list"`, `{"id":"m-0002","probe":"repo.list"`, `"output_bytes":`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("revise task lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, `"output":`) {
		t.Error("the index carries outputs; the role reads what it needs")
	}
	contract := investigationSystemPrompt(ModeDesign, true)
	for _, want := range []string{"Offsets are 0 (the start of any recorded output", "USER_DATA_JSON.earlier_records lists every record of the run so far; read one from offset 0"} {
		if !strings.Contains(contract, want) {
			t.Errorf("revise contract lacks %q", want)
		}
	}
	// The kernel serves offset 0 of an earlier record.
	window, err := input.Session.Read("m-0001", 0)
	if err != nil || window.Offset != 0 || window.Bytes == 0 {
		t.Fatalf("Read(m-0001, 0) = %+v, %v", window, err)
	}
}
