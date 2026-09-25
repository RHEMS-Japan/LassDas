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
// dozen functions and one forgotten call site is a published secret.
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

var (
	mutex    sync.RWMutex
	names    []string
	literals []string
)

// Register records the variable names this card carries and the secret text
// behind them. The text is not always the variable's own value: a credential
// handed over as a path exports the path, which is not secret, while the
// file's contents are.
func Register(variableNames []string, secretText []string) {
	mutex.Lock()
	defer mutex.Unlock()
	for _, name := range variableNames {
		if name != "" && !contains(names, name) {
			names = append(names, name)
		}
	}
	for _, text := range secretText {
		for _, literal := range expand(text) {
			if !contains(literals, literal) {
				literals = append(literals, literal)
			}
		}
	}
	// Longest first, so a value that contains a shorter one is taken out
	// whole rather than in pieces.
	sort.SliceStable(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })
}

// expand is one secret as every form of it that can appear on its own: the
// whole text, and each of its lines. A credentials file is printed a line at
// a time by the tools that read it, and a record holding one of those lines
// has published that line.
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
		if len(line) >= MinLiteralBytes && !contains(forms, line) {
			forms = append(forms, line)
		}
	}
	return forms
}

// FromEnvironment registers what the process that started this one handed
// over. A worker started by a card reads its own environment: the variables
// are already in it, and the name list says which of them are secret.
func FromEnvironment() {
	raw := os.Getenv(NamesEnv)
	if raw == "" {
		return
	}
	var variableNames, secretText []string
	for _, name := range strings.Split(raw, ":") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		variableNames = append(variableNames, name)
		if value := os.Getenv(name); value != "" {
			secretText = append(secretText, value)
		}
	}
	Register(variableNames, secretText)
}

// Names are the variable names this card carries, in the order they were
// registered. An agent launch adds exactly these to the environment it
// builds; a card that carries none adds none.
func Names() []string {
	mutex.RLock()
	defer mutex.RUnlock()
	return append([]string(nil), names...)
}

// Literals are the secret texts, longest first, for a masker that refuses a
// chunk containing any of them.
func Literals() []string {
	mutex.RLock()
	defer mutex.RUnlock()
	return append([]string(nil), literals...)
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

// redactLeadingPartial takes out a value the text begins in the middle of.
// A tail buffer keeps the last so many bytes, and where a single line is
// longer than the whole buffer there is no line boundary to cut on: what
// survives is the end of a secret, which no whole-value replacement finds.
func redactLeadingPartial(text string) string {
	for _, literal := range Literals() {
		// The longest suffix first: a shorter one would leave the rest of
		// the value in place ahead of it.
		for cut := 1; cut+MinLiteralBytes <= len(literal); cut++ {
			suffix := literal[cut:]
			if strings.HasPrefix(text, suffix) {
				return Redacted + text[len(suffix):]
			}
		}
	}
	return text
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// Forget clears the registration. Process-scoped state would otherwise
// carry one test's credentials into the next, and a card is one process:
// nothing in the engine calls this.
func Forget() {
	mutex.Lock()
	defer mutex.Unlock()
	names, literals = nil, nil
}
