package runtime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

// A credential is a secret the operator provisions as a file and the engine
// exports into the cards that were named for it.
//
// The engine is meant to be handed what a delivery needs — a cloud account,
// a database, a registry — rather than to stop and ask for it. A card that
// receives one really receives it: the implementing agent included, because
// a change that has to reach a live service cannot be written, let alone
// verified, by a process that was given no way in. What narrows the reach
// is the list of cards, not any belief about what runs inside one.
//
// The destination token is older than this and stays where it is
// (chain.target_token_path): it is read by two cards by name, and turning
// it into an ordinary entry here would let a configuration widen it by
// editing a list.

// EnvNames is the variable name, or names, one credential is exported
// under. One name is written as a string and several as an array; both
// decode, so the ordinary case reads as the ordinary thing.
type EnvNames []string

func (e *EnvNames) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*e = EnvNames{single}
		return nil
	}
	var several []string
	if err := json.Unmarshal(data, &several); err != nil {
		return errors.New("credential env must be a variable name or a list of them")
	}
	*e = several
	return nil
}

// Credential is one provisioned secret: where its file is, the variable
// names its content is exported under, and the cards that get it.
type Credential struct {
	// Name identifies the credential to a person — in a load refusal, and
	// in the consumer configuration's infrastructure block, which points at
	// one by name. It is never the variable name and never a value.
	Name string `json:"name"`
	// Path is the file the operator provisioned. A file rather than the
	// engine's own environment, for the same reason the destination token
	// is one: the kanban dispatcher spawns every card from one environment,
	// so a value there would reach every card rather than the named ones.
	Path string `json:"path"`
	// Env is what the card's processes see the credential as.
	Env EnvNames `json:"env"`
	// Mode decides what the variable holds. Omitted, and "contents", is the
	// file read into the variable, which is what a token or a connection
	// string wants. "path" exports the file's own name instead, for the
	// tools that take a file rather than a value — a cloud SDK's shared
	// credentials file, a cluster configuration, a service-account key. The
	// contents are still held as secret, because the tool that reads the
	// file prints it when it fails.
	Mode string `json:"mode,omitempty"`
	// Stages are the cards that receive it, by the names the engine
	// dispatches them under.
	Stages []string `json:"stages"`
}

// The two answers mode takes. Omitted reads as CredentialContents.
const (
	CredentialContents = "contents"
	CredentialPath     = "path"
)

// HandsOverPath reports whether the variable holds the file's name rather
// than what is in it.
func (c Credential) HandsOverPath() bool { return c.Mode == CredentialPath }

// maxCredentials bounds the list. It is an operator's own enumeration of
// what a destination needs, not a directory of everything they have.
const maxCredentials = 16

// MaxCredentialBytes bounds one credential file's content. A provisioned
// secret is a token, a connection string or a small credentials file; a
// larger file is a mistaken path, and reading it whole into every named
// card's environment is how a delivery fails with the process table as its
// error message.
const MaxCredentialBytes = 64 * 1024

var (
	// The name appears in refusals and in the run's own record, so it is
	// held to a shape that can be printed as it stands.
	credentialNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// The variable name is exported into a child process; the POSIX shape
	// is what every tool downstream can read back.
	credentialEnvPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
)

// reservedCredentialEnv are the variables a credential may not take,
// besides everything under the two prefixes below. A credential taking one
// of them would not sit beside the engine's value but after it, and the
// last assignment wins: the destination token, or a model key, would
// silently become this file's content, and the card that needs it would
// fail for a reason nothing states. Worse than failing is not failing —
// a credential named LASSDAS_GATEWAY_BASE_URL would quietly send the
// card's model calls somewhere else, which nothing downstream would
// notice.
var reservedCredentialEnv = map[string]bool{
	"TARGET_GITHUB_TOKEN":       true,
	"MODEL_API_KEY_IMPLEMENTER": true,
	"MODEL_API_KEY_REVIEWER":    true,
	"BACKLOG_API_KEY":           true,
	"PATH":                      true,
	"HOME":                      true,
	"LANG":                      true,
	"TMPDIR":                    true,
}

// reservedCredentialPrefixes are the engine's own two namespaces: every
// variable it sets for itself or for the board it runs on begins with one
// of them, so the whole namespace is refused rather than the handful of
// names that exist today.
var reservedCredentialPrefixes = []string{"LASSDAS_", "HERMES_"}

func reservedVariable(name string) bool {
	if reservedCredentialEnv[name] {
		return true
	}
	for _, prefix := range reservedCredentialPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ValidateCredentials is the same check the setup runs before it writes a
// configuration, so a mistyped answer is named where it was written rather
// than by a pod that will not boot.
func ValidateCredentials(credentials []Credential) error { return validateCredentials(credentials) }

// validateCredentials refuses a list that could not mean what it says. A
// duplicated variable name is the one worth naming twice: two credentials
// exporting the same name are not both handed over — one silently replaces
// the other in the card's environment, and which one wins depends on the
// order of a JSON array nobody reads as significant.
func validateCredentials(credentials []Credential) error {
	if len(credentials) > maxCredentials {
		return errors.New("runtime config: chain.credentials holds at most 16 entries")
	}
	names := make(map[string]bool, len(credentials))
	envs := make(map[string]string, len(credentials))
	for _, credential := range credentials {
		if !credentialNamePattern.MatchString(credential.Name) {
			return errors.New("runtime config: chain.credentials[].name is required, in lowercase letters, digits and hyphens")
		}
		if names[credential.Name] {
			return errors.New("runtime config: chain.credentials has two entries named " + credential.Name)
		}
		names[credential.Name] = true
		if credential.Path == "" || !filepath.IsAbs(credential.Path) || strings.ContainsAny(credential.Path, "\r\n\x00") {
			return errors.New("runtime config: chain.credentials[" + credential.Name + "].path must be an absolute file path")
		}
		if len(credential.Env) == 0 {
			return errors.New("runtime config: chain.credentials[" + credential.Name + "].env is required")
		}
		for _, variable := range credential.Env {
			if !credentialEnvPattern.MatchString(variable) {
				return errors.New("runtime config: chain.credentials[" + credential.Name + "].env must be uppercase variable names")
			}
			if reservedVariable(variable) {
				return errors.New("runtime config: chain.credentials[" + credential.Name + "] takes " + variable + ", which the engine sets for its cards itself (LASSDAS_* and HERMES_* are the engine's own, as are PATH, HOME, LANG, TMPDIR and the destination and tracker keys)")
			}
			if owner, taken := envs[variable]; taken {
				return errors.New("runtime config: chain.credentials[" + credential.Name + "] and chain.credentials[" + owner + "] both export " + variable + "; one would silently replace the other")
			}
			envs[variable] = credential.Name
		}
		if credential.Mode != "" && credential.Mode != CredentialContents && credential.Mode != CredentialPath {
			return errors.New(`runtime config: chain.credentials[` + credential.Name + `].mode accepts "contents" (the default) or "path"`)
		}
		if len(credential.Stages) == 0 {
			return errors.New("runtime config: chain.credentials[" + credential.Name + "].stages is required (a credential no card receives is not provisioned at all)")
		}
		seen := make(map[string]bool, len(credential.Stages))
		for _, stage := range credential.Stages {
			if !DispatchedStage(stage) {
				return errors.New("runtime config: chain.credentials[" + credential.Name + "].stages names " + stage + ", which is not a card this engine dispatches (" + strings.Join(DispatchedStages(), ", ") + ")")
			}
			if seen[stage] {
				return errors.New("runtime config: chain.credentials[" + credential.Name + "].stages names " + stage + " twice")
			}
			seen[stage] = true
		}
	}
	return nil
}

// CredentialsFor is what one card receives. A card no credential names gets
// nothing, which is every card of every configuration that declares none.
func (c ChainConfig) CredentialsFor(stage string) []Credential {
	if stage == "" {
		return nil
	}
	var handed []Credential
	for _, credential := range c.Credentials {
		for _, named := range credential.Stages {
			if named == stage {
				handed = append(handed, credential)
				break
			}
		}
	}
	return handed
}

// CredentialNamed finds one by the name the consumer configuration's
// infrastructure block points at.
func (c ChainConfig) CredentialNamed(name string) (Credential, bool) {
	for _, credential := range c.Credentials {
		if credential.Name == name {
			return credential, true
		}
	}
	return Credential{}, false
}

// GuardedFiles is the boot's list of files that must be closed to the agent
// user, given the ones the engine provisions for itself. Every credential
// file joins it.
//
// Without this the list of cards a credential names would be decorative: an
// agent running on a card the credential does not name could open the file
// on disk and have the secret anyway. What narrows the reach is the
// environment plus this; neither alone.
//
// The order is the engine's own files first, then the credentials as the
// configuration declares them, so the string is the same on every boot of
// the same configuration and a local runtime can check its environment
// against it.
func (c ChainConfig) GuardedFiles(engineOwned ...string) string {
	files := append([]string(nil), engineOwned...)
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		seen[file] = true
	}
	for _, credential := range c.Credentials {
		if seen[credential.Path] {
			continue
		}
		seen[credential.Path] = true
		files = append(files, credential.Path)
	}
	return strings.Join(files, ":")
}
