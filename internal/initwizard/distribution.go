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
	CLI              string    `json:"cli"`
	InstalledAt      time.Time `json:"installed_at"`
}

// DistributionFile is where the note lives under the person's home.
const DistributionFile = ".lassdas/distribution.json"

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
	if !strings.Contains(d.Image, "@sha256:") {
		return errors.New("image は registry/name@sha256:digest の形です (タグ名では受け付けません)")
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
