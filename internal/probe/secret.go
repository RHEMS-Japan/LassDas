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
	// Groups: 1 the scheme, 2 the user, 3 the whole credential part up to
	// the last '@' of the run (so a raw '@' inside a password does not
	// leave a tail behind), 4 the password as a URL parser reads it, up to
	// the first '@'. The continuation past the first '@' stops at a closing
	// quote or bracket, so minified JSON keeps its host and its
	// neighbouring fields.
	{"connection string with password", regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*)://([^\s/:@]+):(([^\s@/]{4,})@(?:[^\s@"'<>,)\]}]*@)*)`), true, false},
}

// connectionShape is the index of the connection-string shape, whose
// groups are read by name in pass 1.
const connectionShape = 8

// dbNamePattern reads the path after a connection string's host: its last
// segment is the database name.
var dbNamePattern = regexp.MustCompile(`^[^\s"'<>,)\]}?#]*`)

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
	// masked token it is the rest of a wrapped value. A folded header
	// indents it, a flowed mail leaves a space before the break, and a
	// short tail is still a tail.
	continuation = regexp.MustCompile(`^[ \t]*\r?\n[ \t]*([A-Za-z0-9._~+/-]{4,}=*)[ \t]*(?:\r?\n|$)`)
	// notContinuation is a line the continuation shape would otherwise
	// take for the rest of a token but that is plainly its own line: a
	// number, a version, a date, a rule of dashes.
	notContinuation = regexp.MustCompile(`^(?:[0-9][0-9.-]*|-+|\.+)=*$`)
)

// continuationWords are whole lines the continuation shape would take for
// the rest of a token but that are words of a script or a document.
var continuationWords = map[string]bool{
	"done": true, "else": true, "then": true, "esac": true, "elif": true, "endif": true,
	"true": true, "false": true, "null": true, "none": true, "exit": true, "break": true,
	"return": true, "continue": true, "pass": true, "next": true,
}

// wrappedAfter says the text after a masked token is the rest of it on the
// next line.
func wrappedAfter(rest string) bool {
	match := continuation.FindStringSubmatch(rest)
	if match == nil {
		return false
	}
	line := match[1]
	return !notContinuation.MatchString(line) && !continuationWords[strings.ToLower(strings.TrimRight(line, "="))]
}

// minValueBytes is the shortest value removed from the rest of the output;
// minRunBytes the shortest token run inside a value that is removed on its
// own. How a value is removed from the rest of the output depends on what
// it is:
//   - a default or sample credential (defaultValues, or a password equal to
//     the connection string's own scheme, user or database name, as in
//     postgres://postgres:postgres@db/postgres) is masked in its shape and
//     nowhere else - it is the service's name, not a secret, and removing
//     it would take image names, variables and links with it;
//   - a value with a digit or a symbol in it is removed anywhere, inside
//     other words too;
//   - a letters-only value of shortValueBytes or more is a real, if weak,
//     password and is removed anywhere - a command line (glued to a flag
//     too), a config format, a runbook, a table;
//   - a shorter letters-only value ("data", "root") is removed only where
//     it stands as a word of its own in a value position - right after
//     '=', ':' or a quote - so data_dir and prose stay readable; such a
//     value after a command-line flag (-proot) stays, which is accepted
//     and documented.
const (
	minValueBytes   = 4
	minRunBytes     = 8
	shortValueBytes = 8
)

// defaultValues are the placeholder and default credentials documentation
// and development setups use in place of a real one.
var defaultValues = map[string]bool{
	"password": true, "passwd": true, "changeme": true, "changeit": true, "yourpassword": true,
	"mypassword": true, "placeholder": true, "redacted": true, "example": true, "sample": true,
	"dummy": true, "secret": true, "default": true, "development": true, "production": true,
	"staging": true, "postgres": true, "postgresql": true, "mysql": true, "mariadb": true,
	"redis": true, "rabbitmq": true, "guest": true, "minioadmin": true, "wordpress": true,
	"examplepassword": true, "testpassword": true, "secretpassword": true, "supersecret": true,
	"xxxxxxxx": true, "letmein": true,
}

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

// maskedValue is one form of a value a shape found, the shape that found
// it, and whether it is a default credential (then it is not removed from
// the rest of the output at all).
type maskedValue struct {
	shape  int
	value  string
	sample bool
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
			if shape.token && wrappedAfter(output[match[1]:]) {
				// The value goes on after a line break: nothing bounds it.
				return "", nil, shape.kind
			}
			group := func(g int) string {
				if 2*g+1 < len(match) && match[2*g] >= 0 {
					return output[match[2*g]:match[2*g+1]]
				}
				return ""
			}
			// The value is the token, or for a connection string the
			// password up to the first '@'; its forms are all candidates.
			// The credential part up to the last '@' is a candidate as a
			// whole only, so the host and path it may carry are not. A
			// password that is the connection string's own scheme, user or
			// database name is a default credential.
			primary, outer, sample := output[match[0]:match[1]], "", false
			if i == connectionShape {
				scheme, user, password := group(1), group(2), group(4)
				primary, outer = password, strings.TrimSuffix(group(3), "@")
				dbName := dbNamePattern.FindString(output[match[1]:])
				dbName = dbName[strings.LastIndex(dbName, "/")+1:]
				sample = strings.EqualFold(password, scheme) || strings.EqualFold(password, user) || dbName != "" && strings.EqualFold(password, dbName)
			} else if g := group(1); g != "" {
				primary = g
			}
			sample = sample || defaultValues[strings.ToLower(primary)]
			add := func(candidate string) {
				if len(candidate) >= minValueBytes && !seen[candidate] {
					seen[candidate] = true
					values = append(values, maskedValue{shape: i, value: candidate, sample: sample})
				}
			}
			for _, candidate := range valueCandidates(primary) {
				add(candidate)
			}
			if outer != "" {
				add(outer)
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
		if !v.sample {
			text = replaceValue(text, v.value, placeholder(v.shape))
		}
	}
	// The guard, on the placeholder text: no masked value and nothing
	// shaped may remain.
	for _, v := range values {
		if !v.sample && containsValue(text, v.value) {
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

// lettersOnly says a value is made of ASCII letters alone: a word, not a
// token, and so never removed from inside another word.
func lettersOnly(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// removable says an occurrence of value at [start, end) in text is one the
// rules remove: anywhere for a value with a digit or a symbol (every token
// is one) and for a letters-only value of shortValueBytes or more (a real
// password glued to a flag, -psecretpass, must go too); as a word of its
// own in a value position for a shorter letters-only value.
func removable(text, value string, start, end int) bool {
	if !lettersOnly(value) || len(value) >= shortValueBytes {
		return true
	}
	return wordBounded(text, start, end) && valuePosition(text, start) && !keyPosition(text, end)
}

// replaceValue removes every removable occurrence of value from text.
func replaceValue(text, value, with string) string {
	var out strings.Builder
	for i := 0; ; {
		j := strings.Index(text[i:], value)
		if j < 0 {
			out.WriteString(text[i:])
			return out.String()
		}
		j += i
		if removable(text, value, j, j+len(value)) {
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

// containsValue says whether a removable occurrence of value remains.
func containsValue(text, value string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], value)
		if j < 0 {
			return false
		}
		j += i
		if removable(text, value, j, j+len(value)) {
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

// keyPosition says the text ending at end is a key, not a value: a closing
// quote (if any) and blanks are followed by ':' or "=>", as in
// "password": "..." or 'password' => '...'.
func keyPosition(text string, end int) bool {
	k := end
	if k < len(text) && (text[k] == '"' || text[k] == '\'') {
		k++
	}
	for k < len(text) && (text[k] == ' ' || text[k] == '\t') {
		k++
	}
	return k < len(text) && (text[k] == ':' || strings.HasPrefix(text[k:], "=>"))
}

func wordBounded(text string, start, end int) bool {
	return (start == 0 || !wordByte(text[start-1])) && (end == len(text) || !wordByte(text[end]))
}

func wordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
