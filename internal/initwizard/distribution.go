package initwizard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Distribution is the distributor's note, kept on the machine by
// `lassdas setup install` so the person never pastes it and the agent
// never guesses it: which image runs the body, which source it was built
// from, where that correspondence is recorded, and how the person logs in
// to a private registry. It carries no credential.
type Distribution struct {
	EngineRepository string    `json:"engine_repository"`
	Image            string    `json:"image"`
	EngineSHA        string    `json:"engine_sha"`
	BuildRecord      string    `json:"build_record"`
	RegistryLogin    string    `json:"registry_login,omitempty"`
	CLI              string    `json:"cli,omitempty"`
	InstalledAt      time.Time `json:"installed_at,omitzero"`
}

// DistributionFile is where the note lives under the person's home.
const DistributionFile = ".lassdas/distribution.json"

// RepoDistributionFile is where the body's repository carries the current
// note, so a person (or their AI) handed only the repository's URL finds
// which image runs and where it came from. `lassdas setup note` writes it
// at each release; `lassdas setup install` reads it by default.
const RepoDistributionFile = "docs/DISTRIBUTION.json"

// ReadDistributionFile reads a note wherever it is (the repository's, or
// one given by path) and validates it; CLI and InstalledAt are set by
// install, not here.
func ReadDistributionFile(path string) (Distribution, error) {
	d, err := DecodeDistributionFile(path)
	if err != nil {
		return Distribution{}, err
	}
	if err := d.Validate(); err != nil {
		return Distribution{}, fmt.Errorf("配布者の案内 %s: %v", path, err)
	}
	return d, nil
}

// DecodeDistributionFile reads a note without validating it, so a caller
// can overlay corrections before judging the whole.
func DecodeDistributionFile(path string) (Distribution, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Distribution{}, fmt.Errorf("配布者の案内 %s を読めません: %v", path, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var d Distribution
	if err := decoder.Decode(&d); err != nil {
		return Distribution{}, fmt.Errorf("配布者の案内 %s を読めません: %v", path, err)
	}
	return d, nil
}

// WriteDistributionFile writes a note for the repository (0644, no
// install-time fields), validated first.
func WriteDistributionFile(path string, d Distribution) error {
	d.CLI, d.InstalledAt = "", time.Time{}
	if err := d.Validate(); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// InstalledInstruction is where `setup install` puts the setup
// instruction, beside the documents it links to.
const InstalledInstruction = ".lassdas/SETUP.md"

// distributionAnswers maps the note's fields to the wizard's questions.
var distributionAnswers = []struct {
	id    string
	value func(Distribution) string
}{
	{"engine-repository", func(d Distribution) string { return d.EngineRepository }},
	{"image", func(d Distribution) string { return d.Image }},
	{"engine-sha", func(d Distribution) string { return d.EngineSHA }},
	{"build-record", func(d Distribution) string { return d.BuildRecord }},
}

// Validate refuses a note that would not answer the wizard.
func (d Distribution) Validate() error {
	if !strings.Contains(d.EngineRepository, "/") || strings.ContainsAny(d.EngineRepository, " \t\r\n") {
		return errors.New("engine-repository は owner/name の形です")
	}
	if !imagePattern.MatchString(d.Image) {
		return errors.New("image は registry/name@sha256:<64 桁の小文字 16 進> の形です (タグ名では受け付けません)")
	}
	if login := strings.ToLower(d.RegistryLogin); strings.Contains(login, "--password ") || strings.Contains(login, "--password=") || strings.Contains(login, " -p ") {
		return errors.New("registry-login にパスワードを含めないでください (--password-stdin に別コマンドの出力を渡す形にする)")
	}
	if len(d.EngineSHA) != 40 || strings.Trim(d.EngineSHA, "0123456789abcdef") != "" {
		return errors.New("engine-sha は 40 桁の小文字 16 進です")
	}
	if strings.TrimSpace(d.BuildRecord) == "" {
		return errors.New("build-record (ビルド記録の URL か場所) が必要です")
	}
	return nil
}

// LoadDistribution reads the note; absent is (zero, false), not an error.
func LoadDistribution(home string) (Distribution, bool, error) {
	raw, err := os.ReadFile(filepath.Join(home, filepath.FromSlash(DistributionFile)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Distribution{}, false, nil
		}
		return Distribution{}, false, err
	}
	var d Distribution
	if err := json.Unmarshal(raw, &d); err != nil {
		return Distribution{}, false, fmt.Errorf("%s を読めません: %v", DistributionFile, err)
	}
	if err := d.Validate(); err != nil {
		return Distribution{}, false, fmt.Errorf("%s: %v", DistributionFile, err)
	}
	return d, true, nil
}

// WriteDistribution stores the note (0644: it is not a secret).
func WriteDistribution(home string, d Distribution) error {
	if err := d.Validate(); err != nil {
		return err
	}
	path := filepath.Join(home, filepath.FromSlash(DistributionFile))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(encoded, '\n'), 0o644)
}

// WithDistribution fills the four answers the note carries, where the
// file does not answer them itself. The file wins: a person may point a
// project at another image on purpose.
func (a Answers) WithDistribution(d Distribution) Answers {
	merged := Answers{Answers: map[string]json.RawMessage{}}
	for id, raw := range a.Answers {
		merged.Answers[id] = raw
	}
	for _, entry := range distributionAnswers {
		if _, ok := a.Value(entry.id); ok {
			continue
		}
		if value := entry.value(d); value != "" {
			encoded, _ := json.Marshal(value)
			merged.Answers[entry.id] = encoded
		}
	}
	return merged
}
