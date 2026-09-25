// Package eval measures the reception's decision model against a corpus of
// real requests whose outcome is already known.
//
// It exists only as a test and is never built into anything that ships:
// nothing in the engine imports it, and it does nothing unless an operator
// points it at a corpus and a key. What it answers is whether the fixed
// question set is worth asking at all - how often the model agrees with what
// the reception actually did, how sure it is when it agrees and when it does
// not, what one judgment costs, and how long a run would wait for it.
//
// The corpus is one JSON object per line:
//
//	{"id": "case-1", "text": "the request as the requester wrote it",
//	 "labels": {"kind": "docs", "size": "small", "proceedable": "yes"}}
//
// Labels are optional and partial: a case labels only the questions whose
// answer is known, and agreement is reported over those alone. The corpus
// lives outside this repository - it is made of other people's requests -
// and nothing here prints a request's text, only its id.
//
// A directory is read instead as finished runs: every readiness-ticket.json
// under it is one request, and the decision.json the reception sealed beside
// it says what the reception itself did with that request. That is the only
// label worth having for proceedable, because it is not an opinion about the
// request - it is the verdict this engine actually reached on it. A run the
// reception proceeded on is labelled yes and a run it asked about is
// labelled no; a rejected or unresolved run is labelled nothing, because
// neither answer to this question is what stopped it.
//
// Cases there are numbered, never named. A run directory is named after the
// destination and the ticket it belongs to, and a measurement that printed
// those would copy somebody's tracker into whatever holds its output.
package eval

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/decisions"
)

const (
	// corpusVariable names the file to read. Without it there is nothing to
	// measure and the measurement skips.
	corpusVariable = "LASSDAS_DECISIONS_EVAL_CORPUS"
	// keyNameVariable names the variable the key is in - not the key. The
	// indirection is the same one the destination configuration uses, so
	// nothing here ever holds a key under a name of its own.
	keyNameVariable = "LASSDAS_DECISIONS_EVAL_KEY_ENV"
	// The rest are optional: which model answers, where it is reached, and
	// how many cases to spend.
	modelVariable   = "LASSDAS_DECISIONS_EVAL_MODEL"
	addressVariable = "LASSDAS_DECISIONS_EVAL_BASE_URL"
	limitVariable   = "LASSDAS_DECISIONS_EVAL_LIMIT"

	defaultModel = "typesafe/jev-1.13"
)

type corpusCase struct {
	ID     string            `json:"id"`
	Text   string            `json:"text"`
	Labels map[string]string `json:"labels"`
	// Recorded is what the reception sealed for this request beyond its
	// verdict: how many questions it wrote, and how many points it settled
	// itself. Only a run corpus carries it, and only ever as counts - the
	// questions are prose about somebody's request and never printed.
	Recorded receptionRecord `json:"-"`
}

// receptionRecord is the reception's own record of one request, in numbers.
//
// It is worth having beside the verdict because the verdict alone does not
// say how much was at stake. A request the reception asked one question
// about and settled six points on is not the same case as one it asked
// three questions about and settled nothing, and a threshold read off the
// first kind would be read off the wrong thing.
type receptionRecord struct {
	// Questions is how many the reception put to the requester.
	Questions int
	// Settled is how many points it recorded deciding itself, and Known
	// says whether the corpus holds that record at all: a corpus of sealed
	// decisions alone does not, because the assessment is where they are
	// written, and a zero there means "not in this corpus" rather than
	// "the reception settled nothing".
	Settled int
	Known   bool
}

type judged struct {
	id      string
	answers decisions.Answers
	latency time.Duration
	failure error
}

// TestTheReceptionJudgeAgreesWithTheReception asks the fixed set about every
// request in the corpus and reports what came back. It asserts nothing about
// the model: a threshold asserted here would be a number invented before
// anything had been measured. It fails only when the measurement itself
// could not be taken.
func TestTheReceptionJudgeAgreesWithTheReception(t *testing.T) {
	corpusPath, keyName := os.Getenv(corpusVariable), os.Getenv(keyNameVariable)
	if corpusPath == "" || keyName == "" {
		t.Skipf("set %s to a corpus and %s to the name of the variable holding the key", corpusVariable, keyNameVariable)
	}
	cases := readCorpus(t, corpusPath)
	if limit := os.Getenv(limitVariable); limit != "" {
		count, err := strconv.Atoi(limit)
		if err != nil || count < 1 {
			t.Fatalf("%s = %q is not a number of cases", limitVariable, limit)
		}
		if count < len(cases) {
			cases = cases[:count]
		}
	}
	model := os.Getenv(modelVariable)
	if model == "" {
		model = defaultModel
	}
	client, err := decisions.New(decisions.Endpoint{
		Provider:  "TypeSafe",
		Model:     model,
		BaseURL:   os.Getenv(addressVariable),
		APIKeyEnv: keyName,
	}, &http.Client{})
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}

	questions := decisions.ReceptionQuestions()
	results := make([]judged, 0, len(cases))
	for _, one := range cases {
		started := time.Now()
		answers, err := client.Judge(t.Context(), decisions.NewReceptionState(one.Text), questions)
		results = append(results, judged{id: one.ID, answers: answers, latency: time.Since(started), failure: err})
	}
	report(t, cases, results, questions)
}

func readCorpus(t *testing.T, path string) []corpusCase {
	t.Helper()
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return readRunCorpus(t, path)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	defer file.Close()
	var cases []corpusCase
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var one corpusCase
		if err := json.Unmarshal([]byte(text), &one); err != nil {
			t.Fatalf("corpus line %d is not a case: %v", line, err)
		}
		if one.ID == "" || strings.TrimSpace(one.Text) == "" {
			t.Fatalf("corpus line %d has no id or no text", line)
		}
		cases = append(cases, one)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("the corpus is empty")
	}
	return cases
}

// report prints what was measured. It names a case only by its id: the
// corpus is other people's requests, and a measurement that printed them
// would copy them into whatever holds its output.
func report(t *testing.T, cases []corpusCase, results []judged, questions decisions.Questions) {
	t.Helper()
	labels := make(map[string]map[string]string, len(cases))
	byID := make(map[string]corpusCase, len(cases))
	for _, one := range cases {
		labels[one.ID] = one.Labels
		byID[one.ID] = one
	}
	var failures int
	var cost float64
	var inputTokens int
	latencies := make([]time.Duration, 0, len(results))
	confidences := map[string][]float64{}
	answered := map[string]int{}
	agreed := map[string]int{}
	labelled := map[string]int{}
	chosen := map[string]map[string]int{}
	type disagreement struct {
		id, question, want, got string
		confidence              float64
	}
	var disagreements []disagreement

	for _, result := range results {
		if result.failure != nil {
			failures++
			t.Logf("%s: no judgment: %s", result.id, withoutServiceWords(result.failure))
			continue
		}
		latencies = append(latencies, result.latency)
		cost += result.answers.Usage.Cost
		inputTokens += result.answers.Usage.InputTokens
		for _, id := range decisions.SortedIDs(questions) {
			option, confidence, ok := result.answers.Choice(id)
			if !ok {
				continue
			}
			answered[id]++
			confidences[id] = append(confidences[id], confidence)
			if chosen[id] == nil {
				chosen[id] = map[string]int{}
			}
			chosen[id][option]++
			want, known := labels[result.id][id]
			if !known {
				continue
			}
			labelled[id]++
			if want == option {
				agreed[id]++
				continue
			}
			disagreements = append(disagreements, disagreement{
				id: result.id, question: id, want: want, got: option, confidence: confidence,
			})
		}
	}

	// One line per case, by id. It is what a reader needs to see whether a
	// threshold would have held: an aggregate hides the case that sat one
	// point the wrong side of it.
	t.Log("per case: the option chosen for each question, with its confidence")
	for _, result := range results {
		if result.failure != nil {
			continue
		}
		parts := make([]string, 0, len(questions))
		for _, id := range decisions.SortedIDs(questions) {
			option, confidence, ok := result.answers.Choice(id)
			if !ok {
				parts = append(parts, id+"=-")
				continue
			}
			marked := ""
			if want, known := labels[result.id][id]; known && want != option {
				marked = "!"
			}
			parts = append(parts, fmt.Sprintf("%s=%s(%.2f)%s", id, option, confidence, marked))
		}
		recorded := ""
		if one, known := byID[result.id]; known {
			recorded = fmt.Sprintf("  reception asked %d", one.Recorded.Questions)
			if one.Recorded.Known {
				recorded += fmt.Sprintf(", settled %d", one.Recorded.Settled)
			}
		}
		t.Logf("  %-12s %s%s", result.id, strings.Join(parts, " "), recorded)
	}

	var withLabels int
	for _, one := range cases {
		if len(one.Labels) > 0 {
			withLabels++
		}
	}
	t.Logf("corpus: %d cases, %d carrying labels; %d judged, %d without a judgment",
		len(cases), withLabels, len(results)-failures, failures)

	t.Log("per question: answered / agreed with the label / mean confidence")
	for _, id := range decisions.SortedIDs(questions) {
		line := fmt.Sprintf("  %-14s answered %3d", id, answered[id])
		if labelled[id] > 0 {
			line += fmt.Sprintf("  agreed %2d/%2d (%3.0f%%)", agreed[id], labelled[id],
				100*float64(agreed[id])/float64(labelled[id]))
		} else {
			line += "  agreed  -/ -       "
		}
		line += fmt.Sprintf("  confidence mean %.2f  median %.2f  min %.2f",
			mean(confidences[id]), quantile(confidences[id], 0.5), minimum(confidences[id]))
		t.Log(line)
		t.Logf("      chose %s", distribution(chosen[id]))
	}

	t.Log("confidence distribution, all answers:")
	all := []float64{}
	for _, values := range confidences {
		all = append(all, values...)
	}
	t.Logf("  %s", buckets(all))

	if len(disagreements) > 0 {
		t.Logf("disagreements with the known outcome (%d):", len(disagreements))
		for _, one := range disagreements {
			t.Logf("  %-24s %-14s label %-14s judged %-14s confidence %.2f",
				one.id, one.question, one.want, one.got, one.confidence)
		}
	}

	if len(latencies) > 0 {
		sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
		t.Logf("latency ms: min %d  median %d  p90 %d  max %d",
			latencies[0].Milliseconds(),
			latencies[len(latencies)*5/10].Milliseconds(),
			latencies[min(len(latencies)*9/10, len(latencies)-1)].Milliseconds(),
			latencies[len(latencies)-1].Milliseconds())
	}
	judgedCount := len(results) - failures
	if judgedCount > 0 {
		t.Logf("cost: %.6f in total, %.6f per judgment, %d input tokens per judgment",
			cost, cost/float64(judgedCount), inputTokens/judgedCount)
	}
	// The measurement is the deliverable, so only a measurement that could
	// not be taken is a failure. A model that disagrees with the reception
	// is a finding, and a finding is not a broken test.
	if judgedCount == 0 {
		t.Fatal("no judgment was obtained for any case")
	}
	thresholdTable(t, cases, results)
}

// serviceAnswerPattern matches the one failure that carries words this
// engine did not write: the decisions service's own answer to a call it
// refused.
var serviceAnswerPattern = regexp.MustCompile(`^decisions: the service answered (\d+): `)

// withoutServiceWords keeps the part of a failure this engine wrote and
// drops the part it did not.
//
// A service that refuses a call answers in its own words, and a service
// that refuses a malformed one may quote the field it objected to - which
// here is the state, which is somebody's request. The status is what an
// operator acts on and it is kept; the words are counted and left out.
// Every other failure this measurement can meet is a sentence from the
// client itself, and travels whole.
func withoutServiceWords(failure error) string {
	text := failure.Error()
	if match := serviceAnswerPattern.FindStringIndex(text); match != nil {
		return fmt.Sprintf("%s(the service answered in its own words; %d bytes, not printed here)",
			text[:match[1]], len(text)-match[1])
	}
	return text
}

func distribution(counts map[string]int) string {
	options := make([]string, 0, len(counts))
	for option := range counts {
		options = append(options, option)
	}
	sort.Strings(options)
	parts := make([]string, 0, len(options))
	for _, option := range options {
		parts = append(parts, fmt.Sprintf("%s %d", option, counts[option]))
	}
	return strings.Join(parts, ", ")
}

func buckets(values []float64) string {
	edges := []float64{0.5, 0.7, 0.9, 1.01}
	names := []string{"<0.50", "0.50-0.69", "0.70-0.89", ">=0.90"}
	counts := make([]int, len(edges))
	for _, value := range values {
		for index, edge := range edges {
			if value < edge {
				counts[index]++
				break
			}
		}
	}
	parts := make([]string, 0, len(edges))
	for index, name := range names {
		parts = append(parts, fmt.Sprintf("%s: %d", name, counts[index]))
	}
	return strings.Join(parts, "  ")
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func minimum(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	smallest := values[0]
	for _, value := range values {
		if value < smallest {
			smallest = value
		}
	}
	return smallest
}

func quantile(values []float64, at float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	index := int(at * float64(len(sorted)))
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// The run corpus: one request per finished run, labelled with what the
// reception sealed for it.
//
// Two runs of the same request are one case. A request the reception asked
// about is answered and run again, and that second run reaches ready on the
// strength of the answers - so counting it as a request the reception
// proceeded on would credit the judge for a verdict the requester supplied.
// When a request was ever asked about, asked is its label.
const (
	runTicketFile   = "readiness-ticket.json"
	runDecisionFile = "decision.json"
	// maxRunArtifactBytes bounds one artifact read. These are sealed JSON
	// documents of a few kilobytes; anything larger is a wrong file.
	maxRunArtifactBytes = 1 << 20
)

func readRunCorpus(t *testing.T, root string) []corpusCase {
	t.Helper()
	type collected struct {
		text     string
		runs     int
		asked    bool
		ready    bool
		recorded receptionRecord
	}
	byRequest := map[string]*collected{}
	var order []string
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != runTicketFile {
			return nil
		}
		var ticket struct {
			Request string `json:"request"`
		}
		if readRunArtifact(path, &ticket) != nil || strings.TrimSpace(ticket.Request) == "" {
			return nil
		}
		var decision struct {
			Outcome   string            `json:"outcome"`
			Questions []json.RawMessage `json:"questions"`
		}
		history := filepath.Join(filepath.Dir(path), "history", "readiness")
		if readRunArtifact(filepath.Join(history, runDecisionFile), &decision) != nil {
			return nil
		}
		one, seen := byRequest[ticket.Request]
		if !seen {
			one = &collected{text: ticket.Request}
			byRequest[ticket.Request] = one
			order = append(order, ticket.Request)
		}
		one.runs++
		switch decision.Outcome {
		case "clarification_required":
			one.asked = true
		case "ready":
			one.ready = true
		}
		if count := len(decision.Questions); count > one.recorded.Questions {
			one.recorded.Questions = count
		}
		if settled, known := settledPoints(history); known {
			one.recorded.Known = true
			if settled > one.recorded.Settled {
				one.recorded.Settled = settled
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("reading the corpus: %v", walkErr)
	}
	// Ordered by the request itself, so the same corpus numbers its cases
	// the same way twice running whatever order the filesystem hands them
	// back in - and by a digest, so the order says nothing about the text.
	sort.Slice(order, func(i, j int) bool { return requestKey(order[i]) < requestKey(order[j]) })
	cases := make([]corpusCase, 0, len(order))
	var runs, asked, proceeded int
	for index, request := range order {
		one := byRequest[request]
		runs += one.runs
		labels := map[string]string{}
		switch {
		case one.asked:
			labels[decisions.QuestionProceedable] = decisions.AnswerNo
			asked++
		case one.ready:
			labels[decisions.QuestionProceedable] = decisions.AnswerYes
			proceeded++
		}
		cases = append(cases, corpusCase{ID: fmt.Sprintf("run-%02d", index+1), Text: one.text, Labels: labels, Recorded: one.recorded})
	}
	if len(cases) == 0 {
		t.Fatal("the corpus holds no run with both a request and a sealed decision")
	}
	t.Logf("run corpus: %d runs, %d distinct requests; the reception proceeded on %d and asked about %d, and neither on %d",
		runs, len(cases), proceeded, asked, len(cases)-proceeded-asked)
	var questions, settled, withSettled int
	for _, one := range cases {
		questions += one.Recorded.Questions
		if one.Recorded.Known {
			settled += one.Recorded.Settled
			withSettled++
		}
	}
	if withSettled == 0 {
		t.Logf("the reception recorded %d questions in all; what it settled itself is not in this corpus (the assessments are not here)", questions)
	} else {
		t.Logf("the reception recorded %d questions in all, and %d settled points across the %d cases whose assessment is here",
			questions, settled, withSettled)
	}
	return cases
}

// settledPoints counts what the reception recorded settling itself, newest
// assessment winning, and says whether it found an assessment at all. A
// corpus of sealed decisions alone has none, and reading an absent record
// as a zero would report every request as one the reception settled nothing
// on.
func settledPoints(history string) (int, bool) {
	for attempt := 3; attempt >= 1; attempt-- {
		var assessment struct {
			Assumptions []struct {
				Kind string `json:"kind"`
			} `json:"assumptions"`
		}
		path := filepath.Join(history, fmt.Sprintf("assessment-%d.json", attempt))
		if readRunArtifact(path, &assessment) != nil {
			continue
		}
		count := 0
		for _, assumption := range assessment.Assumptions {
			if assumption.Kind == "defensible_default" {
				count++
			}
		}
		return count, true
	}
	return 0, false
}

// requestKey orders the cases without ordering them by anything a reader of
// the output could turn back into a request.
func requestKey(request string) string {
	sum := sha256.Sum256([]byte(request))
	return hex.EncodeToString(sum[:])
}

func readRunArtifact(path string, out any) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxRunArtifactBytes {
		return errors.New("run artifact unreadable")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return errors.New("run artifact unreadable")
	}
	return json.Unmarshal(encoded, out)
}

// thresholdTable is the measurement the wiring needs and the per-question
// summary cannot give: at each confidence a threshold could be set to, how
// many of the requests the reception proceeded on the judge would agree to
// proceed on, and how many of the ones it asked about the judge would
// overrule. The first number is what the feature buys; the second is what
// it costs, and a threshold is only worth setting where the second is zero.
// logger is what the table writes through: *testing.T in a live pass, and
// something that can be read back in the test that checks the table itself.
type logger interface {
	Helper()
	Log(args ...any)
	Logf(format string, args ...any)
}

func thresholdTable(t logger, cases []corpusCase, results []judged) {
	t.Helper()
	labels := make(map[string]string, len(cases))
	for _, one := range cases {
		labels[one.ID] = one.Labels[decisions.QuestionProceedable]
	}
	type answered struct {
		label, option string
		confidence    float64
	}
	var rows []answered
	for _, result := range results {
		if result.failure != nil {
			continue
		}
		option, confidence, ok := result.answers.Choice(decisions.QuestionProceedable)
		if !ok {
			continue
		}
		rows = append(rows, answered{label: labels[result.id], option: option, confidence: confidence})
	}
	var proceeded, asked int
	for _, row := range rows {
		switch row.label {
		case decisions.AnswerYes:
			proceeded++
		case decisions.AnswerNo:
			asked++
		}
	}
	t.Log("threshold: of the requests the reception proceeded on / asked about, how many the judge answers yes at or above each confidence")
	t.Logf("  %-11s %-22s %s", "threshold", "proceeded, judge yes", "asked, judge yes (the cost)")
	for _, cut := range []float64{0.50, 0.60, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 0.99, 1.00} {
		var agreed, overruled int
		for _, row := range rows {
			if row.option != decisions.AnswerYes || row.confidence < cut {
				continue
			}
			switch row.label {
			case decisions.AnswerYes:
				agreed++
			case decisions.AnswerNo:
				overruled++
			}
		}
		t.Logf("  %-11.2f %3d / %-18d %d / %d", cut, agreed, proceeded, overruled, asked)
	}
	// The lowest confidence at which every request the reception proceeded
	// on is answered yes. Below it the judge would be overruling the
	// reception on requests it was right about; at or above it the judge
	// agrees with every one of them. It is a floor, not the answer: what to
	// set is decided with the cost column beside it.
	lowest := math.Inf(1)
	var missed int
	for _, row := range rows {
		if row.label != decisions.AnswerYes {
			continue
		}
		if row.option != decisions.AnswerYes {
			missed++
			continue
		}
		lowest = math.Min(lowest, row.confidence)
	}
	switch {
	case missed > 0:
		t.Logf("no threshold covers every request the reception proceeded on: the judge answers no on %d of %d", missed, proceeded)
	case math.IsInf(lowest, 1):
		t.Log("no request in this corpus is labelled as one the reception proceeded on")
	default:
		t.Logf("every request the reception proceeded on is answered yes at confidence %.2f and above", lowest)
	}
}

// The labelling is the measurement. A run corpus read with the labels the
// wrong way round would report agreement with the reception while measuring
// disagreement, and nothing downstream would say so.
func TestTheRunCorpusIsLabelledByWhatTheReceptionDid(t *testing.T) {
	root := t.TempDir()
	write := func(destination, run, request, outcome string) {
		t.Helper()
		dir := filepath.Join(root, destination, "runs", run)
		sealed := filepath.Join(dir, "history", "readiness")
		if err := os.MkdirAll(sealed, 0o700); err != nil {
			t.Fatal(err)
		}
		ticket := fmt.Sprintf(`{"request":%q}`, request)
		if err := os.WriteFile(filepath.Join(dir, runTicketFile), []byte(ticket), 0o600); err != nil {
			t.Fatal(err)
		}
		decision := fmt.Sprintf(`{"outcome":%q}`, outcome)
		if err := os.WriteFile(filepath.Join(sealed, runDecisionFile), []byte(decision), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("one", "r1", "proceeded on", "ready")
	write("one", "r2", "asked about", "clarification_required")
	// The same request twice: asked first, then run again once the answers
	// were in. The second run's ready is the requester's doing, not the
	// reception's, so the request is still one the reception asked about.
	write("two", "r3", "asked then answered", "clarification_required")
	write("two", "r4", "asked then answered", "ready")
	write("two", "r5", "refused", "reject")
	// A run with no sealed decision is not a case: nothing says what the
	// reception did with it.
	if err := os.MkdirAll(filepath.Join(root, "two", "runs", "r6"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "two", "runs", "r6", runTicketFile), []byte(`{"request":"undecided"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := readCorpus(t, root)
	if len(cases) != 4 {
		t.Fatalf("read %d cases, want 4", len(cases))
	}
	want := map[string]string{
		"proceeded on":        decisions.AnswerYes,
		"asked about":         decisions.AnswerNo,
		"asked then answered": decisions.AnswerNo,
		"refused":             "",
	}
	for _, one := range cases {
		label, known := want[one.Text]
		if !known {
			t.Errorf("%s: a request nobody wrote was read: %q", one.ID, one.Text)
			continue
		}
		if got := one.Labels[decisions.QuestionProceedable]; got != label {
			t.Errorf("%s: labelled %q, want %q", one.ID, got, label)
		}
		delete(want, one.Text)
	}
	for text := range want {
		t.Errorf("the request labelled %q was not read at all", text)
	}
	// Numbered, never named: a case id that carried the run directory would
	// carry the destination and the ticket with it.
	for index, one := range cases {
		if one.ID != fmt.Sprintf("run-%02d", index+1) {
			t.Errorf("case %d is called %q, want a number", index+1, one.ID)
		}
	}
}

// The measurement runs against other people's requests, so the one failure
// that can carry words this engine did not write must not print them. The
// state travels to the service, and a service refusing a malformed call may
// answer by quoting what it objected to.
func TestAFailureNeverPrintsTheServicesOwnWords(t *testing.T) {
	const request = "レポート画面の合計が合わないので直してほしい"
	refused := fmt.Errorf("decisions: the service answered 400: state.request is invalid: %s", request)
	printed := withoutServiceWords(refused)
	if strings.Contains(printed, request) {
		t.Errorf("the request is in the line the measurement prints: %q", printed)
	}
	if strings.Contains(printed, "state.request is invalid") {
		t.Errorf("the service's own words are printed: %q", printed)
	}
	if !strings.Contains(printed, "400") {
		t.Errorf("the status an operator acts on was dropped: %q", printed)
	}
	// Everything else is a sentence the client wrote, and travels whole so
	// a misconfigured role still says what is wrong with it.
	for _, whole := range []string{
		"decisions: the key variable holds nothing usable",
		"decisions: proceedable came back with no probability",
		"decisions: the call failed: Post \"https://example.com/api/alpha/decisions\": dial tcp: i/o timeout",
	} {
		if got := withoutServiceWords(errors.New(whole)); got != whole {
			t.Errorf("a failure the client wrote was cut: %q", got)
		}
	}
}

// The counts beside each verdict are counts. The questions themselves are
// prose about somebody's request and never leave the corpus.
func TestTheRunCorpusCountsWhatTheReceptionRecordedWithoutQuotingIt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "one", "runs", "r1")
	history := filepath.Join(dir, "history", "readiness")
	if err := os.MkdirAll(history, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "どの画面を直すのか教えてください"
	if err := os.WriteFile(filepath.Join(dir, runTicketFile), []byte(`{"request":"合計を直す"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	decision := `{"outcome":"clarification_required","questions":[{"id":"Q1","question":"` + secret + `"}]}`
	if err := os.WriteFile(filepath.Join(history, runDecisionFile), []byte(decision), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := readCorpus(t, root)
	if len(cases) != 1 {
		t.Fatalf("read %d cases, want 1", len(cases))
	}
	if cases[0].Recorded.Questions != 1 {
		t.Errorf("the reception's question was not counted: %+v", cases[0].Recorded)
	}
	// No assessment beside the decision, so what it settled is unknown -
	// not zero, which would report every request as one it settled nothing
	// on.
	if cases[0].Recorded.Known {
		t.Errorf("a corpus with no assessment reported settled points anyway: %+v", cases[0].Recorded)
	}
	assessment := `{"assumptions":[{"kind":"defensible_default"},{"kind":"repository_convention"},{"kind":"defensible_default"}]}`
	if err := os.WriteFile(filepath.Join(history, "assessment-1.json"), []byte(assessment), 0o600); err != nil {
		t.Fatal(err)
	}
	with := readCorpus(t, root)
	if !with[0].Recorded.Known || with[0].Recorded.Settled != 2 {
		t.Errorf("the points the reception settled were not counted: %+v", with[0].Recorded)
	}
}

// The table is the deliverable of a live pass, and a live pass is the one
// run nobody gets to repeat cheaply. It is exercised here on answers made up
// on the spot, so a mistake in it is found before somebody spends a corpus
// of real calls finding it.
func TestTheThresholdTableReadsTheFloorOffTheAnswers(t *testing.T) {
	answer := func(option string, confidence float64) decisions.Answers {
		picked, sure := option, confidence
		return decisions.Answers{Answers: map[string]decisions.Answer{
			decisions.QuestionProceedable: {Type: decisions.KindChoice, Choice: &picked, Confidence: &sure},
		}}
	}
	cases := []corpusCase{
		{ID: "run-01", Labels: map[string]string{decisions.QuestionProceedable: decisions.AnswerYes}},
		{ID: "run-02", Labels: map[string]string{decisions.QuestionProceedable: decisions.AnswerYes}},
		{ID: "run-03", Labels: map[string]string{decisions.QuestionProceedable: decisions.AnswerNo}},
		{ID: "run-04", Labels: map[string]string{}},
		{ID: "run-05", Labels: map[string]string{decisions.QuestionProceedable: decisions.AnswerYes}},
	}
	results := []judged{
		{id: "run-01", answers: answer(decisions.AnswerYes, 0.94)},
		{id: "run-02", answers: answer(decisions.AnswerYes, 0.81)},
		{id: "run-03", answers: answer(decisions.AnswerYes, 0.55)},
		{id: "run-04", answers: answer(decisions.AnswerNo, 0.99)},
		{id: "run-05", failure: errors.New("decisions: the call failed")},
	}
	lines := &recordingLog{}
	thresholdTable(lines, cases, results)
	// Every request the reception proceeded on that was judged answers yes
	// at 0.81 and above, so 0.81 is the floor; the failed one is not
	// counted as a disagreement, because no judgment was obtained for it.
	if !lines.contains("answered yes at confidence 0.81 and above") {
		t.Errorf("the floor was not read off the answers:\n%s", lines)
	}
	// And the cost column: at 0.50 the one request the reception asked
	// about would be overruled, at 0.70 it would not.
	if !lines.contains("0.50          2 / 2                  1 / 1") {
		t.Errorf("the cost of the lowest threshold is wrong:\n%s", lines)
	}
	if !lines.contains("0.70          2 / 2                  0 / 1") {
		t.Errorf("a threshold above the judged disagreement still shows a cost:\n%s", lines)
	}
	// A judge that answers no on a request the reception proceeded on has
	// no floor at all, and the table says so rather than naming one.
	missed := &recordingLog{}
	thresholdTable(missed, cases, append(append([]judged(nil), results[1:]...),
		judged{id: "run-01", answers: answer(decisions.AnswerNo, 0.99)}))
	if !missed.contains("no threshold covers every request the reception proceeded on") {
		t.Errorf("a judge that overruled the reception was given a floor anyway:\n%s", missed)
	}
}

// recordingLog is the part of *testing.T the table writes through, kept so
// what it prints can be read back instead of only looked at.
type recordingLog struct{ lines []string }

func (r *recordingLog) Helper() {}
func (r *recordingLog) Log(args ...any) {
	r.lines = append(r.lines, fmt.Sprint(args...))
}
func (r *recordingLog) Logf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}
func (r *recordingLog) contains(want string) bool {
	for _, line := range r.lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}
func (r *recordingLog) String() string { return strings.Join(r.lines, "\n") }
