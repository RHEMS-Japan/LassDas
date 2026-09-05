package probe

import (
	"regexp"
	"strings"
)

// secretShapes are the key shapes the kernel refuses to store. Detection is
// the last resort, not the guard: the identities the kernel holds return no
// secret in the first place (docs/INVESTIGATING_DESIGNER.md §3.3, layer 3).
// A hit means the output is not kept and the request is recorded as a
// refusal; masking and keeping would leave the leak's existence in doubt.
var secretShapes = []struct {
	kind    string
	pattern *regexp.Regexp
}{
	{"aws access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"gateway key", regexp.MustCompile(`\bcsk-[A-Za-z0-9_-]{8,}`)},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`)},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"json web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
	{"github token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
	{"chat token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"provider key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"connection string with password", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:[^\s@/]{4,}@`)},
}

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
