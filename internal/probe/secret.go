package probe

import (
	"regexp"
	"strings"
)

// secretShapes are the key shapes the kernel never stores. Detection is
// the last resort, not the guard: the identities the kernel holds return no
// secret in the first place (docs/INVESTIGATING_DESIGNER.md §3.3, layer 3).
//
// A shape whose pattern spans the whole value is masked in place: the value
// is replaced by a marker naming the kind, everything around it is kept,
// and the record says which kinds were masked. Dropping the whole output
// instead left the investigating designer unable to read its own target
// file when one comment in it showed the format of a connection string. A
// shape whose pattern cannot bound the value (a private key: the header is
// matched, the body is not) refuses the whole output, as does any output
// carrying a value the kernel itself holds (the jar's cookie values): those
// are not shaped like a secret, they are one, and their appearance means
// layer 1 leaked.
var secretShapes = []struct {
	kind    string
	pattern *regexp.Regexp
	// whole says the match contains the entire value, so replacing the
	// match removes it; false means a hit refuses the whole output.
	whole bool
}{
	{"aws access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), true},
	{"gateway key", regexp.MustCompile(`\bcsk-[A-Za-z0-9_-]{8,}`), true},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`), true},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), false},
	{"json web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), true},
	{"github token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), true},
	{"chat token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), true},
	{"provider key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), true},
	{"connection string with password", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s@/]{4,}@`), true},
}

// maskMarker is what replaces a masked value: it names the kind, never the
// value, and matches no shape itself.
func maskMarker(kind string) string { return "[masked:" + kind + "]" }

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

// MaskSecrets is the store-time scan. It returns the output with every
// maskable value replaced by its marker and the kinds it masked, in shape
// order without repeats. A non-empty refusal names a shape or value the
// output cannot be stored with at all; the masked text is then empty. The
// masked text is re-scanned before it is returned, so a value that survives
// masking in any form refuses the output rather than being stored.
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
	masked = output
	for _, shape := range secretShapes {
		if !shape.whole || !shape.pattern.MatchString(masked) {
			continue
		}
		masked = shape.pattern.ReplaceAllLiteralString(masked, maskMarker(shape.kind))
		kinds = append(kinds, shape.kind)
	}
	if kind, found := SecretShaped(masked, forbiddenLiterals); found {
		return "", nil, kind
	}
	return masked, kinds, ""
}
