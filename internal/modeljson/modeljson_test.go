package modeljson

import (
	"errors"
	"strings"
	"testing"
)

type answer struct {
	Field    string   `json:"field"`
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
}

// TestAKeyTheShapeDoesNotCarryIsReadPast is the measured failure: a reception
// reader named a gap's field with a word outside the shape and the whole
// answer was thrown away, ending three deliveries inside two minutes each.
// The fields the engine needs are all there.
func TestAKeyTheShapeDoesNotCarryIsReadPast(t *testing.T) {
	var read answer
	if err := Decode([]byte(`{"field":"spec","question":"どちらにしますか","choices":["a","b"],"reason":"本文に無い"}`), &read); err != nil {
		t.Fatalf("an answer with one extra key must be read: %v", err)
	}
	if read.Field != "spec" || read.Question != "どちらにしますか" || len(read.Choices) != 2 {
		t.Fatalf("read = %+v", read)
	}
}

// TestProseAroundTheAnswerIsPeeledOff covers what models do under a response
// schema anyway: a sentence first, a code fence, a sign-off after.
func TestProseAroundTheAnswerIsPeeledOff(t *testing.T) {
	wrapped := map[string]string{
		"a sentence first":     `Here is the JSON: {"field":"spec","question":"q","choices":["a"]}`,
		"a code fence":         "```json\n{\"field\":\"spec\",\"question\":\"q\",\"choices\":[\"a\"]}\n```",
		"a sign-off after":     `{"field":"spec","question":"q","choices":["a"]}` + "\nLet me know if you need more.",
		"prose on both sides":  `I read it as follows. {"field":"spec","question":"q","choices":["a"]} That is my reading.`,
		"a brace in the prose": `The set {a} is meant. {"field":"spec","question":"q","choices":["a"]}`,
	}
	for name, text := range wrapped {
		var read answer
		if err := Decode([]byte(text), &read); err != nil {
			t.Errorf("%s must be read: %v", name, err)
			continue
		}
		if read.Field != "spec" {
			t.Errorf("%s read = %+v", name, read)
		}
	}
}

// TestANeededFieldOfTheWrongTypeIsStillAnError is the boundary: tolerance is
// about shape, not about content. A field the engine reads as text and got an
// object for has nothing in it to read, and the turn asks again.
func TestANeededFieldOfTheWrongTypeIsStillAnError(t *testing.T) {
	var read answer
	if err := Decode([]byte(`{"field":{"name":"spec"},"question":"q","choices":["a"]}`), &read); err == nil {
		t.Fatal("a needed field of the wrong type was accepted")
	}
	if err := Decode([]byte(`{"field":"spec","choices":"a"}`), &read); err == nil {
		t.Fatal("a list field given a string was accepted")
	}
}

func TestTextWithNoJSONValueIsAnError(t *testing.T) {
	var read answer
	err := Decode([]byte("I could not determine which repository this is about."), &read)
	if !errors.Is(err, ErrNoJSONValue) {
		t.Fatalf("prose with no JSON must report %v, got %v", ErrNoJSONValue, err)
	}
	if err := Decode([]byte(`{"field":"spec"`), &read); err == nil {
		t.Fatal("an object that is never closed was accepted")
	}
}

// TestTheFirstCompleteValueIsTheOneRead pins which value is taken when the
// text holds more than one, because the review path relies on knowing: this
// one takes the first, and a caller whose answer sits at the end of a
// transcript locates it itself before calling here.
func TestTheFirstCompleteValueIsTheOneRead(t *testing.T) {
	var read answer
	if err := Decode([]byte(`{"field":"first","question":"q","choices":["a"]} and {"field":"second","question":"q","choices":["a"]}`), &read); err != nil {
		t.Fatal(err)
	}
	if read.Field != "first" {
		t.Fatalf("read = %+v, want the first value", read)
	}
}

// TestAnArrayAnswerIsReadToo: not every answer the engine asks for is an
// object, and the salvage must not be object-only.
func TestAnArrayAnswerIsReadToo(t *testing.T) {
	var read []string
	if err := Decode([]byte(`The list is: ["a","b"]`), &read); err != nil {
		t.Fatalf("an array answer must be read: %v", err)
	}
	if len(read) != 2 || read[0] != "a" {
		t.Fatalf("read = %+v", read)
	}
}

// TestTheScanIsBounded holds the two bounds that keep a pathological answer
// from costing time: the whole answer's length, and how many opening
// delimiters are tried when none of them opens a value.
func TestTheScanIsBounded(t *testing.T) {
	var read answer
	if err := Decode([]byte(strings.Repeat("x", MaxAnswerBytes+1)), &read); err == nil {
		t.Fatal("an answer past the bound was read")
	}
	braces := strings.Repeat("{", 200_000)
	if err := Decode([]byte(braces), &read); err == nil {
		t.Fatal("an answer of nothing but opening braces was read")
	}
}

func TestADestinationMustBeGiven(t *testing.T) {
	if err := Decode([]byte(`{}`), nil); err == nil {
		t.Fatal("a nil destination was accepted")
	}
}

// TestAFragmentBeforeTheAnswerIsSkipped: a model that quoted a cut-off
// example and then answered properly has still answered.
func TestAFragmentBeforeTheAnswerIsSkipped(t *testing.T) {
	var read answer
	if err := Decode([]byte(`For example {"field": ... and my answer is {"field":"spec","question":"q","choices":["a"]}`), &read); err != nil {
		t.Fatalf("an answer after an unclosed fragment must be read: %v", err)
	}
	if read.Field != "spec" {
		t.Fatalf("read = %+v", read)
	}
}
