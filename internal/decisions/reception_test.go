package decisions

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// The set is a contract between the run that asks it and the measurement
// that said it was worth asking. Both read the option ids back, so an id
// renamed on one side and not the other is a judgment silently read as
// something else.
func TestTheReceptionQuestionsAreTheFixedSet(t *testing.T) {
	questions := ReceptionQuestions()
	// Written out, not built from the constants. An id is what an answer
	// comes back under and what a corpus labels a case by, so it is the
	// literal string that is the contract - a test that named the same
	// constant on both sides would follow a rename instead of catching it.
	want := map[string][]string{
		"proceedable":  {"no", "yes"},
		"target_named": {"no", "partly", "yes"},
		"scope_closed": {"no", "yes"},
		"kind":         {"code", "design", "docs", "investigation"},
		"size":         {"large", "medium", "small"},
	}
	for constant, literal := range map[string]string{
		QuestionProceedable: "proceedable", QuestionTargetNamed: "target_named",
		QuestionScopeClosed: "scope_closed", QuestionKind: "kind", QuestionSize: "size",
		AnswerYes: "yes", AnswerNo: "no", AnswerPartly: "partly",
		KindDocs: "docs", KindCode: "code", KindInvestigation: "investigation", KindDesign: "design",
		SizeSmall: "small", SizeMedium: "medium", SizeLarge: "large",
	} {
		if constant != literal {
			t.Errorf("a name callers read answers by moved: %q is now spelled %q", literal, constant)
		}
	}
	if len(questions) != len(want) {
		t.Fatalf("the set holds %d questions, want %d: %v", len(questions), len(want), SortedIDs(questions))
	}
	for id, options := range want {
		question, present := questions[id]
		if !present {
			t.Errorf("%s is not asked", id)
			continue
		}
		// Every one is a choice: a run reads an option id back and acts on
		// it, and a probability or a band index is not something the
		// reception's rules are written in.
		if question.Type != KindChoice {
			t.Errorf("%s is a %s, want a choice", id, question.Type)
			continue
		}
		if strings.TrimSpace(question.Instructions) == "" {
			t.Errorf("%s asks nothing", id)
		}
		criteria, ok := question.Criteria.(map[string]string)
		if !ok {
			t.Errorf("%s offers no options", id)
			continue
		}
		if got := slices.Sorted(maps.Keys(criteria)); !slices.Equal(got, options) {
			t.Errorf("%s offers %v, want %v", id, got, options)
		}
		for option, meaning := range criteria {
			// A criterion is what the model decides by. An option named but
			// not explained is one it has to guess the meaning of, and the
			// whole point of the set is that it does not guess.
			if strings.TrimSpace(meaning) == "" {
				t.Errorf("%s option %s means nothing", id, option)
			}
			if len(meaning) < 40 {
				t.Errorf("%s option %s is explained in %d bytes, too few to decide by", id, option, len(meaning))
			}
		}
	}
}

// The reception's one question that gates the run says, in its own words,
// what the reception's rules say: a point is the requester's only when
// nothing can be read or defended for it.
func TestTheProceedableCriteriaRestateTheReceptionRules(t *testing.T) {
	criteria := ReceptionQuestions()[QuestionProceedable].Criteria.(map[string]string)
	for _, phrase := range []string{"defend", "repository"} {
		if !strings.Contains(strings.ToLower(criteria[AnswerYes]), phrase) {
			t.Errorf("the yes criterion does not mention %q: %q", phrase, criteria[AnswerYes])
		}
	}
	for _, phrase := range []string{"only the requester", "materially different"} {
		if !strings.Contains(strings.ToLower(criteria[AnswerNo]), phrase) {
			t.Errorf("the no criterion does not mention %q: %q", phrase, criteria[AnswerNo])
		}
	}
}

// The set has to be sendable as it stands. A set that fails the client's own
// check would be found on the first live call and not before.
func TestTheReceptionSetIsACallThatCanBeMade(t *testing.T) {
	request := Request{Model: "typesafe/jev-1.13", State: NewReceptionState("add a line"), Questions: ReceptionQuestions()}
	if err := request.Validate(); err != nil {
		t.Fatalf("the reception set cannot be sent: %v", err)
	}
}

// Two callers must not be able to edit each other's copy.
func TestTheReceptionSetIsNotShared(t *testing.T) {
	first := ReceptionQuestions()
	delete(first, QuestionKind)
	first[QuestionSize].Criteria.(map[string]string)[SizeSmall] = "changed"
	second := ReceptionQuestions()
	if _, present := second[QuestionKind]; !present {
		t.Error("deleting a question from one copy removed it from the next")
	}
	if second[QuestionSize].Criteria.(map[string]string)[SizeSmall] == "changed" {
		t.Error("editing one copy's criteria changed the next")
	}
}

// A request in a language whose characters run to several bytes must not be
// cut in the middle of one: the invalid byte is the last thing the service
// reads, and what it does with it is not this engine's to predict.
func TestAnOverlongRequestIsCutOnACharacterBoundary(t *testing.T) {
	for _, testCase := range []struct{ name, unit string }{
		{"plain", "a"},
		{"three bytes per character", "あ"},
		{"four bytes per character", "\U0001F600"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			long := strings.Repeat(testCase.unit, MaxReceptionRequestBytes)
			state := NewReceptionState(long)
			if len(state.Request) > MaxReceptionRequestBytes {
				t.Errorf("the request was not cut: %d bytes", len(state.Request))
			}
			if !utf8.ValidString(state.Request) {
				t.Error("the request was cut in the middle of a character")
			}
			if len(long) > MaxReceptionRequestBytes && state.Request == long {
				t.Error("an overlong request travelled whole")
			}
		})
	}
	short := "add a line"
	if NewReceptionState(short).Request != short {
		t.Error("a request inside the bound was changed")
	}
}
