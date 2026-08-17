package worker

import (
	"automation.internal/ticket-ingress/internal/hook"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readinessFixtureFile is the shared inventory for the readiness gate. The
// unit tests below prove every fixture is a well-formed executable ticket and
// that the pipeline maps each expected assessor decision to the right gate
// outcome. The real-model preflight (issue #8) must run the same fixtures
// against the configured endpoints before ticket intake is enabled.
const readinessFixtureFile = "testdata/readiness_fixtures.json"

type readinessFixtureSet struct {
	SchemaVersion int                `json:"schema_version"`
	Fixtures      []readinessFixture `json:"fixtures"`
}

type readinessFixture struct {
	Name                      string   `json:"name"`
	RequestBody               string   `json:"request_body"`
	SourceContent             string   `json:"source_content"`
	ExpectedDecision          string   `json:"expected_decision"`
	ExpectedRejectCode        string   `json:"expected_reject_code"`
	AllowedDimensions         []string `json:"allowed_dimensions"`
	MaxQuestions              int      `json:"max_questions"`
	WithResolvedClarification bool     `json:"with_resolved_clarification,omitempty"`
	Note                      string   `json:"note"`
}

func loadReadinessFixtures(t *testing.T) []readinessFixture {
	t.Helper()
	encoded, err := os.ReadFile(readinessFixtureFile)
	if err != nil {
		t.Fatal(err)
	}
	var set readinessFixtureSet
	if err := decodeStrictJSON(encoded, &set); err != nil {
		t.Fatalf("fixture file is invalid: %v", err)
	}
	if set.SchemaVersion != 1 {
		t.Fatalf("fixture schema version = %d", set.SchemaVersion)
	}
	return set.Fixtures
}

func fixtureDescription(body string) string {
	return strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Target-File: client/src/components/Example.tsx",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		body,
	}, "\n")
}

type scriptedResponse struct {
	text      string
	requestID string
}

type scriptedChatAPI struct {
	responses []scriptedResponse
	index     int
	prompts   []string
}

func (s *scriptedChatAPI) ChatCompletions(_ context.Context, _ ModelEndpoint, request ChatRequest) (*ChatResponse, error) {
	if s.index >= len(s.responses) {
		return nil, errors.New("no scripted response remains")
	}
	for _, message := range request.Messages {
		if message.Role == "user" {
			s.prompts = append(s.prompts, message.Content)
		}
	}
	response := s.responses[s.index]
	s.index++
	return &ChatResponse{
		ID: response.requestID,
		Choices: []ChatChoice{{
			FinishReason: ChatFinishStop,
			Message:      ChatMessage{Role: "assistant", Content: response.text},
		}},
		Usage: &ChatUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}, nil
}

func expectedAssessorResponse(t *testing.T, fixture readinessFixture) string {
	t.Helper()
	switch fixture.ExpectedDecision {
	case ReadinessOutcomeReady:
		return `{"decision":"ready","questions":[],"assumptions":[{"kind":"non_user_visible_implementation","statement":"Constant naming follows the existing file convention.","evidence":"Existing declarations in client/src/components/Example.tsx"}],"reject_code":""}`
	case ReadinessOutcomeClarification:
		return `{"decision":"clarification_required","questions":[{"id":"Q1","dimension":"preapproved_scope_choice","question":"Which occurrences of the wording should change?","why_blocking":"The visible result differs by chosen scope.","choices":[{"id":"a","label":"Heading only","effect":"The submit button keeps the old wording."},{"id":"b","label":"Both occurrences","effect":"Heading and submit button both show the new wording."}]}],"assumptions":[],"reject_code":""}`
	case ReadinessOutcomeReject:
		return `{"decision":"reject","questions":[],"assumptions":[],"reject_code":"` + fixture.ExpectedRejectCode + `"}`
	case ReadinessOutcomeUnresolved:
		return `{"decision":"unresolvable","questions":[],"assumptions":[],"reject_code":""}`
	default:
		t.Fatalf("fixture %q has an unsupported expected decision %q", fixture.Name, fixture.ExpectedDecision)
		return ""
	}
}

func TestReadinessFixturesCoverRequiredSet(t *testing.T) {
	required := []string{"clear", "ambiguous-scope", "source-resolvable", "implementation-detail", "out-of-scope", "secret-request", "prompt-injection", "resolved-clarification-ready"}
	fixtures := loadReadinessFixtures(t)
	byName := make(map[string]readinessFixture, len(fixtures))
	for _, fixture := range fixtures {
		if _, duplicate := byName[fixture.Name]; duplicate {
			t.Fatalf("fixture %q is duplicated", fixture.Name)
		}
		byName[fixture.Name] = fixture
	}
	for _, name := range required {
		if _, exists := byName[name]; !exists {
			t.Fatalf("required fixture %q is missing", name)
		}
	}
	if len(fixtures) != len(required) {
		t.Fatalf("fixture count = %d, want %d", len(fixtures), len(required))
	}
}

func TestReadinessFixturesAreExecutableAndGateCorrectly(t *testing.T) {
	for _, fixture := range loadReadinessFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			config := validTestConfig()
			request, err := ParseTicket(validTicketEnvelope(t, fixtureDescription(fixture.RequestBody)), config)
			if err != nil {
				t.Fatalf("fixture ticket does not parse: %v", err)
			}
			root := t.TempDir()
			filename := filepath.Join(root, "client", "src", "components", "Example.tsx")
			if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, []byte(fixture.SourceContent), 0o600); err != nil {
				t.Fatal(err)
			}
			source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
			if err != nil {
				t.Fatalf("fixture source does not snapshot: %v", err)
			}

			api := &scriptedChatAPI{responses: []scriptedResponse{
				{text: expectedAssessorResponse(t, fixture), requestID: "request-fixture-assessor"},
				{text: `{"verdict":"pass","reasons":[]}`, requestID: "request-fixture-checker"},
			}}
			invoker, err := NewModelInvoker(api)
			if err != nil {
				t.Fatal(err)
			}
			var clarification *ClarificationContext
			if fixture.WithResolvedClarification {
				clarification = fixtureClarification(t, request)
			}
			assessment, _, err := invoker.AssessReadiness(context.Background(), 1, nil, nil, clarification, source, request, config)
			if err != nil {
				t.Fatalf("AssessReadiness() error = %v", err)
			}
			check, _, err := invoker.CheckReadiness(context.Background(), assessment, clarification, source, request, config)
			if err != nil {
				t.Fatalf("CheckReadiness() error = %v", err)
			}
			decision, err := DecideReadiness([]ReadinessAssessment{assessment}, []ReadinessCheck{check}, source, request, config)
			if err != nil {
				t.Fatalf("DecideReadiness() error = %v", err)
			}
			if decision.Outcome != fixture.ExpectedDecision || decision.RejectCode != fixture.ExpectedRejectCode {
				t.Fatalf("decision = %+v, want %s/%s", decision, fixture.ExpectedDecision, fixture.ExpectedRejectCode)
			}
			if fixture.WithResolvedClarification {
				// Both model calls must actually see the resolved answers
				// inside USER_DATA_JSON, and the assessment must be sealed to
				// the exact clarification bytes it was shown.
				if len(api.prompts) < 2 {
					t.Fatalf("captured prompts = %d, want at least 2", len(api.prompts))
				}
				for index, prompt := range api.prompts[:2] {
					if !strings.Contains(prompt, "resolved_clarification") ||
						!strings.Contains(prompt, "Which occurrences of the wording should change?") ||
						!strings.Contains(prompt, `"Q1":"b"`) {
						t.Fatalf("prompt %d lacks the resolved answers:\n%s", index, prompt)
					}
				}
				if assessment.ClarificationSHA256 != clarification.SHA256 {
					t.Fatalf("assessment clarification digest = %s, want %s", assessment.ClarificationSHA256, clarification.SHA256)
				}
			}
			if fixture.ExpectedDecision == ReadinessOutcomeClarification && len(decision.Questions) == 0 {
				t.Fatal("clarification decision carried no questions")
			}
			if len(decision.Questions) > fixture.MaxQuestions {
				t.Fatalf("decision carried %d questions, fixture allows %d", len(decision.Questions), fixture.MaxQuestions)
			}
			for _, question := range decision.Questions {
				allowed := false
				for _, dimension := range fixture.AllowedDimensions {
					allowed = allowed || question.Dimension == dimension
				}
				if !allowed {
					t.Fatalf("question dimension %q is not allowed by the fixture", question.Dimension)
				}
			}
			if fixture.ExpectedDecision != ReadinessOutcomeReady {
				forged := decision
				if _, _, err := invoker.GenerateCandidate(context.Background(), 1, forged, nil, source, request, nil, nil, config); err == nil {
					t.Fatal("GenerateCandidate() accepted a non-ready fixture decision")
				}
			}
		})
	}
}

// fixtureClarification builds the sealed round-1 exchange for the
// resolved-clarification fixture: the ambiguous-scope question answered with
// "both occurrences" (Q1:b), sealed exactly as the resume path would seal it.
func fixtureClarification(t *testing.T, request TicketRequest) *ClarificationContext {
	t.Helper()
	questionsJSON := `[{"id":"Q1","dimension":"preapproved_scope_choice","question":"Which occurrences of the wording should change?","why_blocking":"The visible result differs by chosen scope.","choices":[{"id":"a","label":"Heading only","effect":"The submit button keeps the old wording."},{"id":"b","label":"Both occurrences","effect":"Heading and submit button both show the new wording."}]}]`
	question := hook.QuestionRecord{
		Protocol:          hook.QuestionProtocolVersion,
		DeliveryID:        request.DeliveryID,
		InputSHA256:       request.InputSHA256,
		RepositoryID:      42,
		RepositorySHA256:  hook.HashIdentity("example/automation-receiver"),
		WorkflowRefSHA256: hook.HashIdentity("example/automation-receiver/.github/workflows/m1-worker.yml@refs/heads/main"),
		WorkflowSHA:       strings.Repeat("2", 40),
		WorkflowRunID:     123456789,
		RunAttempt:        1,
		AutomationRunID:   request.RunID,
		RunURL:            "https://github.com/example/automation-receiver/actions/runs/123456789/attempts/1",
		QuestionRevision:  1,
		QuestionsJSON:     questionsJSON,
		QuestionsSHA256:   hook.TerminalReportDigest([]byte(questionsJSON)),
		DecisionSHA256:    strings.Repeat("c", 64),
		AnswerDeadlineAt:  4_000,
		NotifyAt:          [3]int64{1_000, 2_000, 3_000},
	}
	encodedQuestion, err := hook.MarshalQuestionRecord(question)
	if err != nil {
		t.Fatalf("fixture question does not seal: %v", err)
	}
	answers := `{"Q1":"b"}`
	record := hook.ClarificationRecord{
		Protocol:          hook.ClarificationProtocolVersion,
		DeliveryID:        question.DeliveryID,
		InputSHA256:       question.InputSHA256,
		RepositoryID:      question.RepositoryID,
		RepositorySHA256:  question.RepositorySHA256,
		WorkflowRefSHA256: question.WorkflowRefSHA256,
		AutomationRunID:   question.AutomationRunID,
		InputRevision:     2,
		Rounds: []hook.ClarificationRound{{
			QuestionRecordJSON:   string(encodedQuestion),
			QuestionRecordSHA256: hook.TerminalReportDigest(encodedQuestion),
			QuestionCommentID:    500,
			AnswerCommentID:      600,
			AnswererID:           7,
			AnswerPostedAt:       3_500,
			AnswerBodySHA256:     strings.Repeat("b", 64),
			AnswersJSON:          answers,
			AnswersSHA256:        hook.TerminalReportDigest([]byte(answers)),
		}},
	}
	encoded, err := hook.MarshalClarificationRecord(record)
	if err != nil {
		t.Fatalf("fixture clarification does not seal: %v", err)
	}
	clarification, err := DecodeClarificationContext(encoded)
	if err != nil {
		t.Fatalf("fixture clarification does not decode: %v", err)
	}
	return clarification
}
