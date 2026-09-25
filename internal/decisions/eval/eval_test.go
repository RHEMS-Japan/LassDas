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
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
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
	for _, one := range cases {
		labels[one.ID] = one.Labels
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
			t.Logf("%s: no judgment: %v", result.id, result.failure)
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
		t.Logf("  %-12s %s", result.id, strings.Join(parts, " "))
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
