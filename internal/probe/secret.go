package probe

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// secretShapes are the key shapes the kernel never stores. Detection is
// the last resort, not the guard: the identities the kernel holds return no
// secret in the first place (docs/INVESTIGATING_DESIGNER.md §3.3, layer 3).
//
// A shape whose pattern bounds the value is masked in place: the match is
// replaced by a marker naming the kind, everything around it is kept, and
// the record says which kinds were masked. The value itself, in every form
// it can take (valueCandidates), is then removed wherever else it appears
// in the output, in any context: a token that shows up in a request header
// and again in a response body, a password exported on its own line and
// again inside a connection string, a key id inside a file name. Dropping
// the whole output instead left the investigating designer unable to read
// its own target file when one comment in it showed the format of a
// connection string.
//
// Detection is at least as wide as it was when a hit refused the whole
// output: a bearer value runs to the next whitespace, a password to the
// first '@'. What the match swallows beyond the value (a closing quote, a
// Markdown or HTML delimiter) is masked with it, which is the safe
// direction, and the candidates strip it back off so the bare value is
// still found elsewhere.
//
// A shape whose pattern cannot bound the value refuses the whole output: a
// private key (the header is matched, the body is not), a token that
// continues on the next line (wrappedValue), and any output carrying a
// value the kernel itself holds (the jar's cookie values) - those are not
// shaped like a secret, they are one, and their appearance means layer 1
// leaked.
var secretShapes = []struct {
	kind    string
	pattern *regexp.Regexp
	// whole says the match bounds the value, so replacing the match (and
	// every other occurrence of the value) removes it; false means a hit
	// refuses the whole output.
	whole bool
	// token says the value is an opaque token that a log or a mail may
	// wrap onto the next line; such a continuation refuses the output.
	token bool
}{
	{"aws access key id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), true, false},
	{"gateway key", regexp.MustCompile(`\bcsk-[A-Za-z0-9_-]{8,}`), true, true},
	{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+(\S{16,})`), true, true},
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`), false, false},
	{"json web token", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), true, true},
	{"github token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`), true, true},
	{"chat token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`), true, true},
	{"provider key", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`), true, true},
	// Group 1 is the whole credential part up to the last '@' of the run
	// (so a raw '@' inside a password does not leave a tail behind); group
	// 2 is the password as a URL parser reads it, up to the first '@'. The
	// continuation past the first '@' stops at a closing quote or bracket,
	// so minified JSON keeps its host and its neighbouring fields.
	{"connection string with password", regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/:@]+:(([^\s@/]{4,})@(?:[^\s@"'<>,)\]}]*@)*)`), true, false},
}

// maskMarker is what replaces a masked value: it names the kind (hyphenated,
// so that a marker followed by other text can never spell a shape the way
// "bearer token]..." would), never the value. markerPattern finds markers
// so a scan can look through them.
func maskMarker(kind string) string { return "[masked:" + strings.ReplaceAll(kind, " ", "-") + "]" }

var markerPattern = regexp.MustCompile(`\[masked:[a-z-]+\]`)

// markerBlank is what a scan sees in place of a marker or a placeholder: a
// character that belongs to no shape, with whitespace on both sides so the
// text on either side of it cannot run together into a shape.
const markerBlank = " � "

// placeholder stands in for a masked match or value while the output is
// checked: it carries no kind words, so a masked value can never be found
// again inside what replaced it. The private-use runes it is built from are
// replaced in the output first (privateUse), so nothing in the output can
// be mistaken for one.
func placeholder(index int) string { return "" + strconv.Itoa(index) + "" }

var (
	placeholderPattern = regexp.MustCompile("[0-9]+")
	privateUse         = strings.NewReplacer("", "�", "", "�")
	// tokenRun is a maximal run of the characters a bearer token or a key
	// is made of (RFC 6750 b64token); a value's runs are candidates too.
	tokenRun = regexp.MustCompile(`[A-Za-z0-9._~+/=-]+`)
	// continuation is a line that is nothing but token characters (with
	// '=' only as trailing padding, so a KEY=value line is not one): after a
	// masked token it is the rest of a wrapped value.
	continuation = regexp.MustCompile(`^\r?\n[A-Za-z0-9._~+/-]{8,}=*(?:\r?\n|$)`)
)

// minValueBytes is the shortest value removed from the rest of the output;
// minRunBytes the shortest token run inside a value that is removed on its
// own. Values shorter than unboundedValueBytes that are letters only (a
// sample password like "password" or "changeme") are removed only where
// they stand as a word of their own in a value position - right after '=',
// ':' or a quote, as in PGPASSWORD=changeme or "password": "changeme" -
// so prose ("set the password") and keys ("password:") stay readable; a
// value with a digit or a symbol in it, or a long one, is removed anywhere.
const (
	minValueBytes       = 4
	minRunBytes         = 8
	unboundedValueBytes = 16
)

// SecretShaped reports whether output carries a key-shaped string or any of
// the literal values the caller knows must never appear (the jar's cookie
// values, for instance). Mask markers are looked through: each is read as
// a hard boundary (markerBlank), so a marker that happens to follow
// "Bearer " or "user:" is not itself a shape and cannot join the text after
// it into one. The returned kind names the shape, never the value.
func SecretShaped(output string, forbiddenLiterals []string) (string, bool) {
	output = markerPattern.ReplaceAllLiteralString(output, markerBlank)
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

// maskedValue is one form of a value a shape found, and the shape that
// found it.
type maskedValue struct {
	shape int
	value string
}

// valueCandidates are the forms of a captured value that are removed from
// the rest of the output: the value as captured; the value without the
// closing punctuation or delimiter a pattern may have swallowed (sentence
// punctuation too, since '.' is a token character but "TOKEN." at the end
// of a sentence is TOKEN); every run of token characters inside it that is
// long enough to be a value of its own (so "TOKEN</code" still yields
// TOKEN); and the part before the first '@' (for a connection string, the
// password as a URL parser reads it).
func valueCandidates(value string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		for _, form := range []string{v, strings.TrimRight(v, ".,;:!?")} {
			if len(form) >= minValueBytes && !seen[form] {
				seen[form] = true
				out = append(out, form)
			}
		}
	}
	add(value)
	add(strings.TrimRightFunc(value, func(r rune) bool { return !tokenRune(r) }))
	if at := strings.Index(value, "@"); at > 0 {
		add(value[:at])
	}
	for _, run := range tokenRun.FindAllString(value, -1) {
		if len(run) >= minRunBytes {
			add(run)
		}
	}
	return out
}

func tokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._~+/=-", r)
}

// MaskSecrets is the store-time scan. It returns the output with every
// maskable match replaced by its marker, every masked value removed from
// wherever else it appeared, and the kinds it masked in shape order without
// repeats. A non-empty refusal names a shape or value the output cannot be
// stored with at all; the masked text is then empty. Before the markers go
// in, the text is checked with placeholders in their place - for every
// masked value and for every shape - and refused if anything survived; the
// final text is checked for shapes once more, looking through the markers.
func MaskSecrets(output string, forbiddenLiterals []string) (masked string, kinds []string, refusal string) {
	output = privateUse.Replace(output)
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
	seen := map[string]bool{}
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
			if shape.token && continuation.MatchString(output[match[1]:]) {
				// The value goes on after a line break: nothing bounds it.
				return "", nil, shape.kind
			}
			captured := []string{output[match[0]:match[1]]}
			for g := 2; g+1 < len(match); g += 2 {
				if match[g] >= 0 {
					captured = append(captured, output[match[g]:match[g+1]])
				}
			}
			for _, value := range captured[1:] {
				captured = append(captured, strings.TrimSuffix(value, "@"))
			}
			if len(captured) == 1 {
				captured = captured[:1]
			} else {
				captured = captured[1:]
			}
			for _, value := range captured {
				for _, candidate := range valueCandidates(value) {
					if !seen[candidate] {
						seen[candidate] = true
						values = append(values, maskedValue{shape: i, value: candidate})
					}
				}
			}
		}
	}
	if len(kinds) == 0 {
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
		text = replaceValue(text, v.value, placeholder(v.shape))
	}
	// The guard, on the placeholder text: no masked value and nothing
	// shaped may remain.
	for _, v := range values {
		if containsValue(text, v.value) {
			return "", nil, secretShapes[v.shape].kind
		}
	}
	if kind, found := SecretShaped(placeholderPattern.ReplaceAllLiteralString(text, markerBlank), forbiddenLiterals); found {
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

// unbounded says a value is removed wherever it occurs: it is long, or it
// carries a digit or a symbol and so is not a word prose would use.
func unbounded(value string) bool {
	if len(value) >= unboundedValueBytes {
		return true
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// replaceValue removes every occurrence of value from text: anywhere for an
// unbounded value, only where it stands as a word of its own (no letter,
// digit or underscore on either side) in a value position otherwise.
func replaceValue(text, value, with string) string {
	if unbounded(value) {
		return strings.ReplaceAll(text, value, with)
	}
	var out strings.Builder
	for i := 0; ; {
		j := strings.Index(text[i:], value)
		if j < 0 {
			out.WriteString(text[i:])
			return out.String()
		}
		j += i
		if wordBounded(text, j, j+len(value)) && valuePosition(text, j) {
			out.WriteString(text[i:j])
			out.WriteString(with)
			i = j + len(value)
			continue
		}
		_, size := utf8.DecodeRuneInString(text[j:])
		out.WriteString(text[i : j+size])
		i = j + size
	}
}

// containsValue says whether value still occurs in text under the same rule
// replaceValue removes it by.
func containsValue(text, value string) bool {
	if unbounded(value) {
		return strings.Contains(text, value)
	}
	for i := 0; ; {
		j := strings.Index(text[i:], value)
		if j < 0 {
			return false
		}
		j += i
		if wordBounded(text, j, j+len(value)) && valuePosition(text, j) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[j:])
		i = j + size
	}
}

// valuePosition says the text at start is where a value is written: the
// nearest non-blank character before it on the same line is '=', ':' or a
// quote. A word at the start of a line or after other words is prose or a
// key.
func valuePosition(text string, start int) bool {
	for k := start - 1; k >= 0; k-- {
		switch text[k] {
		case ' ', '\t':
			continue
		case '=', ':', '"', '\'', '`':
			return true
		default:
			return false
		}
	}
	return false
}

func wordBounded(text string, start, end int) bool {
	return (start == 0 || !wordByte(text[start-1])) && (end == len(text) || !wordByte(text[end]))
}

func wordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
