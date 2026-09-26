// Package modeljson reads a model's answer.
//
// Everything a model answers used to be decoded the way the engine decodes
// its own sealed records: one JSON value and nothing else, every object key
// known to the struct, duplicates refused. That is right for a record this
// engine wrote and wrong for an answer a model wrote, because the two fail
// for opposite reasons. A record with a key the reader does not know was
// written by another engine and must not be trusted; an answer with a key
// the reader does not need was written by a model being helpful, and the
// fields the engine asked for are all there.
//
// The cost of reading the second like the first was measured: three
// requests that genuinely lacked information — the ones the reception's
// single round of questions exists for — died at the reception inside two
// minutes because the model named a gap's field with a word outside the
// shape, and a stronger model named it the same way. Nothing was wrong with
// the answers. The engine refused them.
//
// So a model's answer is read for what the engine needs and nothing more:
// keys the struct does not carry are ignored, and prose or a code fence
// around the JSON is peeled off by taking the first complete object or
// array in the text. What stays an error is the only thing that cannot be
// worked around — a needed field whose value is not the type the engine
// reads, and text with no JSON value in it at all.
//
// Nothing here loosens a safety check. Path scope, allow-lists, key names,
// forbidden text and size bounds are applied to what the fields say, after
// this package has read them, exactly as before.
package modeljson

import (
	"bytes"
	"encoding/json"
	"errors"
)

// MaxAnswerBytes bounds what this package will look through for a JSON
// value. Every caller already bounds the answer it accepts, at a small
// fraction of this; the bound is here so the scan below can never be handed
// an unbounded string by a caller added later.
const MaxAnswerBytes = 8 << 20

// maxSalvageAttempts bounds how many opening delimiters are tried when the
// answer is not itself one JSON value. A model that wrapped its answer in
// prose or a fence put one object in the text and the first delimiter finds
// it; the bound is what keeps a pathological answer of nothing but braces
// from costing time quadratic in its length.
const maxSalvageAttempts = 16

// ErrNoJSONValue is what an answer with no readable JSON value in it
// reports. It is a sentinel so a caller can tell "the model wrote no JSON"
// from "the model wrote JSON the engine cannot use", which are different
// things to tell a reader.
var ErrNoJSONValue = errors.New("the answer carries no JSON value")

// Decode reads what the engine needs out of one model answer.
//
// Unknown object keys are ignored. An answer wrapped in prose, a code fence
// or both is read by taking the first complete JSON object or array in it. A
// needed field whose value is the wrong type is still an error, and so is an
// answer with no JSON value in it.
func Decode(encoded []byte, destination any) error {
	if destination == nil {
		return errors.New("model answer destination is invalid")
	}
	value, err := OneJSONValue(encoded)
	if err != nil {
		return err
	}
	// Decoded once, from bytes already chosen: a first attempt that failed
	// part way through would leave half a struct behind for the second to
	// decode over, and the caller would be holding fields from two readings
	// of one answer.
	return json.NewDecoder(bytes.NewReader(value)).Decode(destination)
}

// DecodeAnswer reads the last complete object carrying a role's answer key.
// Earlier examples and later unrelated metadata are not the answer. Selection
// precedes decoding, so a bad type in the final answer cannot resurrect an
// earlier example or leave fields from two different answers in destination.
func DecodeAnswer(encoded []byte, destination any, keys ...string) error {
	if destination == nil || len(keys) == 0 {
		return errors.New("model answer destination or keys are invalid")
	}
	if len(encoded) > MaxAnswerBytes {
		return errors.New("the answer is too large to read")
	}
	// Preserve the ordinary single-value path, including its type errors.
	if trimmed := bytes.TrimSpace(encoded); json.Valid(trimmed) {
		return Decode(trimmed, destination)
	}
	var selected []byte
	attempts := 0
	for start := 0; start < len(encoded); start++ {
		if encoded[start] != '{' && encoded[start] != '[' {
			continue
		}
		attempts++
		if attempts > maxSalvageAttempts {
			// Choosing a known earlier example when the tail was not read
			// would be worse than asking the model for a shorter answer.
			return errors.New("the answer has too many JSON fragments")
		}
		end, closed := balancedEnd(encoded, start)
		if !closed || !json.Valid(encoded[start:end]) {
			continue
		}
		value := encoded[start:end]
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) == nil {
			for _, key := range keys {
				if _, present := fields[key]; present {
					selected = value
					break
				}
			}
		}
		// A nested example or metadata object cannot replace its container.
		start = end - 1
	}
	if selected == nil {
		return ErrNoJSONValue
	}
	return Decode(selected, destination)
}

// OneJSONValue is the bytes Decode reads: the answer itself when the whole
// of it is one JSON value, and otherwise the first complete object or array
// found inside it.
//
// It is exported because a caller that has already located the answer inside
// a longer transcript — an agent writes prose, then its verdict — has its own
// rule for which value is the answer, and needs to say so rather than have
// this one applied underneath it.
func OneJSONValue(encoded []byte) ([]byte, error) {
	if len(encoded) > MaxAnswerBytes {
		return nil, errors.New("the answer is too large to read")
	}
	if trimmed := bytes.TrimSpace(encoded); json.Valid(trimmed) {
		return trimmed, nil
	}
	if value, found := firstJSONValue(encoded); found {
		return value, nil
	}
	return nil, ErrNoJSONValue
}

// firstJSONValue returns the first complete JSON object or array in the
// text. Each candidate is found by walking forward to the delimiter that
// closes the one that opened it, ignoring delimiters inside strings so prose
// with a brace in it cannot end an object early, and is then checked as
// JSON — counting delimiters proves nothing about the value between them.
func firstJSONValue(text []byte) ([]byte, bool) {
	attempts := 0
	for start := 0; start < len(text) && attempts < maxSalvageAttempts; start++ {
		if text[start] != '{' && text[start] != '[' {
			continue
		}
		attempts++
		// A delimiter that never closes is skipped rather than ending the
		// scan: a model that quoted a cut-off fragment and then answered
		// properly has still answered, and the attempt bound above is what
		// keeps the skipping cheap.
		if end, closed := balancedEnd(text, start); closed && json.Valid(text[start:end]) {
			return text[start:end], true
		}
	}
	return nil, false
}

// balancedEnd returns the offset just past the delimiter that closes the one
// at start.
func balancedEnd(text []byte, start int) (int, bool) {
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(text); index++ {
		character := text[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return index + 1, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}
