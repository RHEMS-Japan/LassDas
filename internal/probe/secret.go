package probe

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// secretShapes are the key shapes the kernel never stores. Detection is
// the last resort, not the guard: the identities the kernel holds return no
// secret in the first place (docs/INVESTIGATING_DESIGNER.md §3.3, layer 3).
//
// A shape whose pattern bounds the value is masked in place: the match is
// replaced by a marker naming the kind, everything around it is kept, and
// the record says which kinds were masked. The value itself (the capture
// group when the pattern has one, the whole match otherwise) is then
// removed wherever else it appears in the output, in any context: a token
// that shows up in a request header and again in a response body, a
// password exported on its own line and again inside a connection string,
// a key id inside a file name. Dropping the whole output instead left the
// investigating designer unable to read its own target file when one
// comment in it showed the format of a connection string.
//
// A shape whose pattern cannot bound the value (a private key: the header
// is matched, the body is not) refuses the whole output, as does any output
// carrying a value the kernel itself holds (the jar's cookie values): those
// are not shaped like a secret, they are one, and their appearance means
// layer 1 leaked.
var secretShapes = []struct {
	kind    string
	pattern *regexp.Regexp
	// whole says the match bounds the value, so replacing the match (and
	// every other occurrence of the value) removes it; false means a hit
	// refuses the whole output.
	whole bool
}{
	{"aws access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), true},
	{"gateway key", regexp.MustCompile(`\bcsk-[A-Za-z0-9_-]{8,}`), true},
	// The token runs to the next whitespace: its alphabet is the issuer's
	// choice, and a token with a ':' or a quote in it must not leave its
	// tail behind.
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+(\S{16,})`), true},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), false},
	{"json web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), true},
	{"github token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), true},
	{"chat token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), true},
	{"provider key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), true},
	// The password runs to the last '@' before whitespace, so a password
	// with an '@' in it is taken whole; when a URL carries a later '@' the
	// host goes with it, which is the safe direction.
	{"connection string with password", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:(\S{4,})@`), true},
}

// maskMarker is what replaces a masked value: it names the kind (hyphenated,
// so that a marker followed by other text can never spell a shape the way
// "bearer token]..." would), never the value.
func maskMarker(kind string) string { return "[masked:" + strings.ReplaceAll(kind, " ", "-") + "]" }

// placeholder stands in for a masked match or value while the output is
// checked: it carries no kind words, so a masked value can never be found
// again inside what replaced it.
func placeholder(index int) string { return "\uE000" + strconv.Itoa(index) + "\uE001" }

// SecretShaped reports whether output carries a key-shaped string or any of
// the literal values the caller knows must never appear (the jar's cookie
// values, for instance). The returned kind names the shape, never the value.
func SecretShaped(output string, forbiddenLiterals []string) (string, bool) {
	for _, shape := range secretShapes {
		if shape.pattern.MatchString(output) {
			return shape.kind, true
		}
	}
	for _, literal := range forbiddenLiterals {
		if len(literal) >= 8 && strings.Contains(output, literal) {
			return "known secret value", true
		}
	}
	return "", false
}

// maskedValue is one value a shape found, and the shape that found it.
type maskedValue struct {
	shape int
	value string
}

// MaskSecrets is the store-time scan. It returns the output with every
// maskable match replaced by its marker, every masked value removed from
// wherever else it appeared, and the kinds it masked in shape order without
// repeats. A non-empty refusal names a shape or value the output cannot be
// stored with at all; the masked text is then empty. Before the markers go
// in, the text is checked with placeholders in their place - for every
// masked value and for every shape - and refused if anything survived; the
// final text is checked for shapes once more.
func MaskSecrets(output string, forbiddenLiterals []string) (masked string, kinds []string, refusal string) {
	for _, literal := range forbiddenLiterals {
		if len(literal) >= 8 && strings.Contains(output, literal) {
			return "", nil, "known secret value"
		}
	}
	for _, shape := range secretShapes {
		if !shape.whole && shape.pattern.MatchString(output) {
			return "", nil, shape.kind
		}
	}
	// Pass 1: the values, found on the original text.
	var values []maskedValue
	for i, shape := range secretShapes {
		if !shape.whole {
			continue
		}
		matches := shape.pattern.FindAllStringSubmatchIndex(output, -1)
		if len(matches) == 0 {
			continue
		}
		kinds = append(kinds, shape.kind)
		for _, match := range matches {
			value := output[match[0]:match[1]]
			if len(match) >= 4 && match[2] >= 0 {
				value = output[match[2]:match[3]]
			}
			values = append(values, maskedValue{shape: i, value: value})
		}
	}
	if len(values) == 0 {
		return output, nil, ""
	}
	// Pass 2: the matches, in their context.
	text := output
	for i, shape := range secretShapes {
		if shape.whole {
			text = shape.pattern.ReplaceAllLiteralString(text, placeholder(i))
		}
	}
	// Pass 3: the values, wherever else they appear. Longer values first,
	// so a value that contains another is removed whole.
	sort.SliceStable(values, func(i, j int) bool { return len(values[i].value) > len(values[j].value) })
	for _, v := range values {
		text = strings.ReplaceAll(text, v.value, placeholder(v.shape))
	}
	// The guard, on the placeholder text: no masked value and nothing
	// shaped may remain.
	for _, v := range values {
		if strings.Contains(text, v.value) {
			return "", nil, secretShapes[v.shape].kind
		}
	}
	if kind, found := SecretShaped(text, forbiddenLiterals); found {
		return "", nil, kind
	}
	masked = text
	for i, shape := range secretShapes {
		masked = strings.ReplaceAll(masked, placeholder(i), maskMarker(shape.kind))
	}
	if kind, found := SecretShaped(masked, forbiddenLiterals); found {
		return "", nil, kind
	}
	return masked, kinds, ""
}
