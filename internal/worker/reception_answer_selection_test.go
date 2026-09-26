package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

func TestAuditReceptionUsesTheFinalAnswer(t *testing.T) {
	t.Run("assessment", func(t *testing.T) {
		example := `{"decision":"clarification_required","questions":[` + oneDraftedQuestion + `],"assumptions":[],"reject_code":""}`
		final := `{"decision":"ready","questions":[],"assumptions":[],"reject_code":""}`
		got, err := DecodeModelReadinessOutput([]byte("Format example: " + example + "\nMy actual answer: " + final))
		if err != nil || got.Decision != "ready" || len(got.Questions) != 0 {
			t.Fatalf("final answer lost: %+v, %v", got, err)
		}
	})
	t.Run("check", func(t *testing.T) {
		got, err := DecodeModelReadinessCheckOutput([]byte(`Example: {"verdict":"fail","reasons":[]} Answer: {"verdict":"pass","reasons":[]} Metadata: {"note":"done"}`))
		if err != nil || got.Verdict != "pass" {
			t.Fatalf("final answer lost: %+v, %v", got, err)
		}
	})
	t.Run("intake", func(t *testing.T) {
		got, err := DecodeModelIntakeOutput([]byte(`Example: {"request":"example","gaps":[]} Answer: {"request":"actual request","gaps":[]}`))
		if err != nil || got.Request != "actual request" {
			t.Fatalf("final answer lost: %+v, %v", got, err)
		}
	})
}

func TestAuditRefusalWithoutAnIdentifierIsStillRead(t *testing.T) {
	for _, reason := range []string{"", "この依頼は対象外です", "unresolvable"} {
		t.Run(reason, func(t *testing.T) {
			encoded, _ := json.Marshal(reason)
			answer := `{"decision":"reject","questions":null,"assumptions":null,"reject_code":` + string(encoded) + `}`
			decision, _, _, _ := decisionFor(t, answer)
			if decision.Outcome != ReadinessOutcomeReady || decision.Fallback {
				t.Fatalf("refusal became an unread answer: %+v", decision)
			}
			if strings.TrimSpace(decision.RejectedReading) == "" {
				t.Fatal("the refusal was not recorded")
			}
			if reason != "" && decision.RejectedReading != reason {
				t.Fatalf("refusal wording lost: %q", decision.RejectedReading)
			}
		})
	}
}

func TestAuditFinalAnswerTypeErrorDoesNotResurrectTheExample(t *testing.T) {
	_, err := DecodeModelReadinessOutput([]byte(`Example: {"decision":"ready"} Answer: {"decision":["ready"]}`))
	if err == nil {
		t.Fatal("the unreadable final answer was replaced by the example")
	}
}

func TestAuditRefusalStillHonorsTextAndSizeBounds(t *testing.T) {
	for _, reason := range []string{strings.Repeat("x", 65), "bad\x00reason", "bad\nreason", " bad "} {
		if err := validateModelReadinessOutput(ModelReadinessOutput{Decision: ReadinessOutcomeReject, RejectCode: reason}); err == nil {
			t.Fatalf("unsafe refusal reason was accepted: %q", reason)
		}
	}
}

func TestAuditOtherRolesUseTheFinalAnswer(t *testing.T) {
	t.Run("candidate", func(t *testing.T) {
		got, err := DecodeModelCandidateOutput([]byte(`Example: {"files":[],"rationale":"example"} Answer: {"files":[],"rationale":"actual"}`))
		if err != nil || got.Rationale != "actual" {
			t.Fatalf("candidate=%+v err=%v", got, err)
		}
	})
	t.Run("arbitration", func(t *testing.T) {
		config, request, source, candidate, reviews := nonconvergedFixture(t)
		invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput("Example: " + overrulingAnswer(objectingSeat(config), request.TargetFiles[0]) + " Answer: " + instructingAnswer)})
		got, err := invoker.Arbitrate(context.Background(), candidate, reviews, nil, nil, source, request, config, testInvocationTime)
		if err != nil || got.Ruling != RulingInstructImplementer {
			t.Fatalf("ruling=%+v err=%v", got, err)
		}
	})
	t.Run("investigation turn", func(t *testing.T) {
		got, err := decodeTurnAnswer([]byte(`Example: {"read":{"id":"example","offset":0}} Answer: {"report":{"questions":["actual"]}}`), ModeInvestigation)
		if err != nil || got.Read != nil || !strings.Contains(string(got.Report), "actual") {
			t.Fatalf("turn=%+v err=%v", got, err)
		}
	})
	t.Run("investigation report", func(t *testing.T) {
		got, err := investigate.DecodeModelInvestigationOutput([]byte(`Example: {"questions":["example"]} Answer: {"questions":["actual"]}`))
		if err != nil || len(got.Questions) != 1 || got.Questions[0] != "actual" {
			t.Fatalf("report=%+v err=%v", got, err)
		}
	})
	t.Run("design", func(t *testing.T) {
		got, err := investigate.DecodeModelDesignOutput([]byte(`Example: {"cause":"example"} Answer: {"cause":"actual"}`))
		if err != nil || got.Cause != "actual" {
			t.Fatalf("design=%+v err=%v", got, err)
		}
	})
	t.Run("design review", func(t *testing.T) {
		got, err := investigate.DecodeModelDesignReviewOutput([]byte(`Example: {"verdict":"revise"} Answer: {"verdict":"pass"}`))
		if err != nil || got.Verdict != "pass" {
			t.Fatalf("review=%+v err=%v", got, err)
		}
	})
	t.Run("preflight", func(t *testing.T) {
		config := validTestConfig()
		invoker, _ := NewModelInvoker(&fakeChatAPI{output: chatOutput(`Example: {"status":"not-ready"} Answer: {"status":"ready"}`)})
		if _, err := invoker.Preflight(context.Background(), config.Models.Implementer); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAuditReadinessKeepsQuestionsDespiteTheDecisionLabel(t *testing.T) {
	for _, label := range []string{"ready", "unresolvable", "clarification_required"} {
		t.Run(label, func(t *testing.T) {
			question := strings.Replace(oneDraftedQuestion, `"Q1"`, `"Q99"`, 1)
			decision, _, _, _ := decisionFor(t, `{"decision":"`+label+`","questions":[`+question+`],"assumptions":[]}`)
			if decision.Fallback || decision.Outcome != ReadinessOutcomeClarification || len(decision.Questions) != 1 || decision.Questions[0].ID != "Q1" {
				t.Fatalf("a real question was discarded: %+v", decision)
			}
		})
	}
}

func TestAuditAnEmptyClarificationIsNotAnUnreadAnswer(t *testing.T) {
	decision, _, _, _ := decisionFor(t, `{"decision":"clarification_required","questions":null,"assumptions":[]}`)
	if decision.Fallback || decision.Outcome != ReadinessOutcomeReady {
		t.Fatalf("decision=%+v", decision)
	}
}
