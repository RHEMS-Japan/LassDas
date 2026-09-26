package modeljson

import (
	"strings"
	"testing"
)

func TestRoleAnswerSelection(t *testing.T) {
	for name, input := range map[string]string{
		"prose":           `Example: {"field":"example","question":"must not leak"} Answer: {"field":"actual"}`,
		"metadata":        `Example: {"field":"example"} Answer: {"field":"actual"} Metadata: {"note":{"field":"nested example"}}`,
		"escaped braces":  `Example: {"field":"example"} Answer: {"field":"actual","ignored":"a \" } {"}`,
		"broken fragment": `Example {"field": ... Actual: {"field":"actual"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got answer
			if err := DecodeAnswer([]byte(input), &got, "field"); err != nil || got.Field != "actual" || got.Question != "" {
				t.Fatalf("answer=%+v err=%v", got, err)
			}
		})
	}
}

func TestRoleAnswerSelectionDoesNotHideTypeErrorsOrSizeBounds(t *testing.T) {
	for name, input := range map[string]string{
		"type":          `Example: {"field":"example"} Answer: {"field":[]}`,
		"size":          strings.Repeat(" ", MaxAnswerBytes) + `{"field":"actual"}`,
		"fragments":     strings.Repeat(`{"field":"example"} `, maxSalvageAttempts+1) + `{"field":"actual"}`,
		"only metadata": `Here: {"note":"done"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got answer
			if err := DecodeAnswer([]byte(input), &got, "field"); err == nil {
				t.Fatalf("unsafe reading accepted: %+v", got)
			}
		})
	}
}
