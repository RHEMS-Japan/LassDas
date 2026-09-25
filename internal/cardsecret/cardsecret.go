// Package cardsecret holds the credential values one card was handed, for
// the length of that card's process, so that everything which writes text a
// person may later read can keep them out of it.
//
// It is process-scoped state, which is unusual here and deliberate. The
// values are a property of the card, not of any one call: the live log's
// sink is opened deep inside a step, the agent's transcript is captured in
// another package again, and a card's work spans two processes — the runner
// that dispatches the step and the worker it starts. Threading the values
// through every one of those call sites would mean a new parameter on a
// dozen functions, and one forgotten call site is a published secret.
//
// A card that was handed nothing registers nothing, which is every card of
// every configuration that declares no credentials.
package cardsecret

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// NamesEnv carries the variable names a card's credentials were exported
// as, colon-separated, from the process that read the files to the
// processes it starts. The values travel as the variables themselves; this
// says which of them are secret, which is the one thing a child process
// cannot work out for itself.
const NamesEnv = "LASSDAS_CREDENTIAL_ENV"

// PathNamesEnv is the subset of those variables that hold a file's name
// rather than what is in it. The two are handled differently everywhere
// downstream — a file name is not secret and must not be masked out of a
// build log, and a file an AI is given the name of has to be one it can
// open — so which is which travels with the names.
const PathNamesEnv = "LASSDAS_CREDENTIAL_PATH_ENV"

// Redacted is what a value reads as once it is out of the card. The
// variable's name is not printed with it: what matters downstream is that
// something was taken out, and naming the variable in a record that travels
// to a ticket would say which secret a destination holds.
const Redacted = "[secret]"

// MinLiteralBytes is the shortest value worth taking out of text. Anything
// shorter matches ordinary words in build output, and a record where every
// third word reads [secret] says nothing about why a card failed, which is
// the only reason the record exists. Every credential this engine is handed
// — a token, a connection string, a key file — is far longer.
const MinLiteralBytes = 8

// Entry is one variable a card carries.
type Entry struct {
	// Name is the variable the card exports it as.
	Name string
	// Secret is the text that must never be published: the value for a
	// credential handed over as contents, and the file's contents for one
	// handed over as a path. Empty where the caller does not hold it.
	Secret string
	// Path marks a variable that holds a file's name. The name itself is
	// not secret.
	Path bool
}

// secret is one text to keep out of everything, and the variable to name
// when a gate refuses over it.
type secret struct{ text, owner string }

var (
	mutex     sync.RWMutex
	names     []string
	pathNames map[string]bool
	secrets   []secret
	// scanned are credentials this process holds only to check that none
	// of them got into a change. They are kept apart from secrets on
	// purpose: what a card carries decides what its processes receive and
	// what is masked out of its logs, and neither may widen because a gate
	// elsewhere needed to read more.
	scanned []secret
)

// Register records what this card carries. It is additive and idempotent:
// a card registers once when it reads the files, and a process it starts
// registers again from its own environment.
func Register(entries []Entry) {
	mutex.Lock()
	defer mutex.Unlock()
	if pathNames == nil {
		pathNames = map[string]bool{}
	}
	for _, entry := range entries {
		if entry.Name == "" {
			continue
		}
		if !contains(names, entry.Name) {
			names = append(names, entry.Name)
		}
		if entry.Path {
			pathNames[entry.Name] = true
		}
		for _, text := range expand(entry.Secret) {
			if !held(secrets, text) {
				secrets = append(secrets, secret{text: text, owner: entry.Name})
			}
		}
	}
	// Longest first, so a value that contains a shorter one is taken out
	// whole rather than in pieces.
	sort.SliceStable(secrets, func(i, j int) bool { return len(secrets[i].text) > len(secrets[j].text) })
}

// RegisterForScan records a credential the engine holds only to check that
// its value did not end up in a change.
//
// The card that seals a round is not always the card the credential was
// handed to — the usual configuration hands one to the card that writes the
// change, and another card seals it — so a check that could only see what
// its own card received would never look at the ordinary case. Handing the
// value to the sealing card instead would be worse: that is what the list
// of stages exists to prevent. So the sealing worker, which is the engine's
// own process and not the AI's, reads the files itself and registers them
// here.
//
// Nothing registered this way reaches an agent's environment, a validation
// sandbox or a mask: Names and Literals do not see it. Only VariableIn
// does.
func RegisterForScan(entries []Entry) {
	mutex.Lock()
	defer mutex.Unlock()
	for _, entry := range entries {
		for _, text := range expand(entry.Secret) {
			if !held(scanned, text) && !held(secrets, text) {
				scanned = append(scanned, secret{text: text, owner: entry.Name})
			}
		}
	}
	sort.SliceStable(scanned, func(i, j int) bool { return len(scanned[i].text) > len(scanned[j].text) })
}

// expand is one secret as every form of it that can appear on its own: the
// whole text, and each of its lines. A credentials file is printed a line
// at a time by the tools that read it, and a record holding one of those
// lines has published that line.
func expand(text string) []string {
	var forms []string
	if len(text) >= MinLiteralBytes {
		forms = append(forms, text)
	}
	if !strings.ContainsAny(text, "\r\n") {
		return forms
	}
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if len(line) >= MinLiteralBytes && !containsText(forms, line) {
			forms = append(forms, line)
		}
	}
	return forms
}

// FromEnvironment registers what the process that started this one handed
// over. A worker started by a card reads its own environment: the variables
// are already in it, and the name lists say which of them are secret and
// which hold a file name.
func FromEnvironment() {
	raw := os.Getenv(NamesEnv)
	if raw == "" {
		return
	}
	paths := map[string]bool{}
	for _, name := range strings.Split(os.Getenv(PathNamesEnv), ":") {
		if name = strings.TrimSpace(name); name != "" {
			paths[name] = true
		}
	}
	var entries []Entry
	for _, name := range strings.Split(raw, ":") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		entry := Entry{Name: name, Path: paths[name]}
		if !entry.Path {
			entry.Secret = os.Getenv(name)
		}
		entries = append(entries, entry)
	}
	Register(entries)
}

// Names are the variable names this card carries, in the order they were
// registered. An agent launch adds exactly these to the environment it
// builds; a card that carries none adds none.
func Names() []string {
	mutex.RLock()
	defer mutex.RUnlock()
	return append([]string(nil), names...)
}

// HandedAsPath reports whether a variable holds a file's name.
func HandedAsPath(name string) bool {
	mutex.RLock()
	defer mutex.RUnlock()
	return pathNames[name]
}

// Literals are the secret texts, longest first, for a masker that refuses a
// chunk containing any of them.
func Literals() []string {
	mutex.RLock()
	defer mutex.RUnlock()
	out := make([]string, 0, len(secrets))
	for _, held := range secrets {
		out = append(out, held.text)
	}
	return out
}

// VariableIn names the variable whose secret text appears in the given
// text, or "" when none does. It is for the gates that refuse rather than
// redact: what such a gate has to say is which credential got out, and
// saying the value would publish it again in the refusal.
func VariableIn(text string) string {
	if text == "" {
		return ""
	}
	mutex.RLock()
	defer mutex.RUnlock()
	for _, group := range [][]secret{secrets, scanned} {
		for _, held := range group {
			if strings.Contains(text, held.text) {
				return held.owner
			}
		}
	}
	return ""
}

// Redact replaces every registered secret in text. It is for the records
// this engine keeps and wants to stay readable: a failure record exists to
// say why a card failed, so the secret goes and the sentence around it
// stays.
func Redact(text string) string {
	if text == "" {
		return text
	}
	for _, literal := range Literals() {
		text = strings.ReplaceAll(text, literal, Redacted)
	}
	return redactLeadingPartial(text)
}

// redactLeadingPartial takes out a value a line begins in the middle of.
// A tail buffer keeps the last so many bytes, and where a single line is
// longer than the whole buffer there is no boundary to cut on: what
// survives is the end of a secret, which no whole-value replacement finds.
//
// Every line, not only the first: the tail says above the kept text that
// its first line starts part-way through, so the part-value is no longer at
// the start of the text.
func redactLeadingPartial(text string) string {
	literals := Literals()
	if len(literals) == 0 {
		return text
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		for _, literal := range literals {
			// The longest suffix first: a shorter one would leave the rest
			// of the value in place ahead of it.
			for cut := 1; cut+MinLiteralBytes <= len(literal); cut++ {
				if suffix := literal[cut:]; strings.HasPrefix(line, suffix) {
					lines[index] = Redacted + line[len(suffix):]
					break
				}
			}
			if lines[index] != line {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsText(values []string, want string) bool { return contains(values, want) }

func held(values []secret, want string) bool {
	for _, value := range values {
		if value.text == want {
			return true
		}
	}
	return false
}

// Forget clears the registration. Process-scoped state would otherwise
// carry one card's credentials into the next, and a card is one process:
// nothing in the engine calls this.
func Forget() {
	mutex.Lock()
	defer mutex.Unlock()
	names, pathNames, secrets, scanned = nil, nil, nil, nil
}
