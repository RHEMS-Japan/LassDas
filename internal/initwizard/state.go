// Package initwizard prepares one local instance from externally obtained keys.
// Its journal contains answers and observations, never credentials or their hashes.
package initwizard

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker"
)

var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,47}$`)
var stages = []string{"prepare", "consumer", "tracker", "models", "runtime", "smoke"}

type State struct {
	Version               int                             `json:"version"`
	Project               string                          `json:"project"`
	RepoRoot              string                          `json:"repo_root"`
	Repository            string                          `json:"repository"`
	RepositoryID          int64                           `json:"repository_id"`
	DefaultBranch         string                          `json:"default_branch"`
	Branch                string                          `json:"branch"`
	BaseSHA               string                          `json:"base_sha"`
	EngineRepository      string                          `json:"engine_repository"`
	EngineRepositoryID    int64                           `json:"engine_repository_id"`
	EngineSHA             string                          `json:"engine_sha"`
	BuildRecord           string                          `json:"build_record"`
	Image                 string                          `json:"image"`
	DockerContext         string                          `json:"docker_context"`
	Pins                  map[string]string               `json:"pins"`
	Mode                  worker.ModeConfig               `json:"mode"`
	Tracker               runtimeconfig.TrackerConfig     `json:"tracker"`
	Category              string                          `json:"category"`
	StatusNames           [4]string                       `json:"status_names"`
	Models                map[string]worker.ModelEndpoint `json:"models"`
	BaseURL               string                          `json:"base_url"`
	ModelKeyMode          string                          `json:"model_key_mode,omitempty"`
	SeparateDesignReviews bool                            `json:"separate_design_reviews"`
	BoardPort             int                             `json:"board_port"`
	AutomationRunID       string                          `json:"automation_run_id"`
	Completed             map[string]string               `json:"completed"`
	Checks                map[string]json.RawMessage      `json:"checks"`
	Smoke                 json.RawMessage                 `json:"smoke,omitempty"`
	Metrics               Metrics                         `json:"metrics"`
}

type Metrics struct {
	Fields                 int     `json:"fields"`
	Defaulted              int     `json:"defaulted"`
	Additional             int     `json:"additional"`
	Reentered              int     `json:"reentered"`
	ActiveSeconds          float64 `json:"active_seconds"`
	DownloadSeconds        float64 `json:"download_seconds"`
	ExternalKeyAcquisition string  `json:"external_key_acquisition"`
}

// Secrets includes only runtime credentials. Keys entered only for a single
// administrator or smoke operation do not belong here. A requester key may
// also serve as BACKLOG_API_KEY after explicit confirmation of runtime use.
type Secrets map[string]string

func ProjectDir(home, project string) (string, error) {
	if !projectPattern.MatchString(project) {
		return "", errors.New("project は英小文字で始まる 2〜48 文字の英小文字・数字・ハイフンです")
	}
	return filepath.Join(home, ".lassdas", project), nil
}

// LoadState reads only the nonsecret journal. Runtime control uses it so a
// broken credential file cannot prevent stopping the owned container.
func LoadState(dir string) (*State, error) {
	state := &State{Version: 1, Models: map[string]worker.ModelEndpoint{}, Pins: map[string]string{}, Completed: map[string]string{}, Checks: map[string]json.RawMessage{}}
	raw, err := os.ReadFile(filepath.Join(dir, "init.json"))
	if err == nil {
		if json.Unmarshal(raw, state) != nil || state.Version != 1 {
			return nil, errors.New("init 台帳の形式が違います。旧 setup 台帳は移行しません")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if state.Models == nil {
		state.Models = map[string]worker.ModelEndpoint{}
	}
	if state.Pins == nil {
		state.Pins = map[string]string{}
	}
	if state.Completed == nil {
		state.Completed = map[string]string{}
	}
	if state.Checks == nil {
		state.Checks = map[string]json.RawMessage{}
	}
	return state, nil
}

func Load(dir string) (*State, Secrets, error) {
	state, err := LoadState(dir)
	if err != nil {
		return nil, nil, err
	}
	secrets := Secrets{}
	raw, err := os.ReadFile(filepath.Join(dir, "runtime.env"))
	if err == nil {
		info, statErr := os.Lstat(filepath.Join(dir, "runtime.env"))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, nil, errors.New("runtime.env は通常ファイル・0600 である必要があります")
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line == "" {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok || !envName.MatchString(k) {
				return nil, nil, errors.New("runtime.env の形式が違います")
			}
			secrets[k] = v
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	return state, secrets, nil
}

func Save(dir string, state *State, secrets Secrets) error {
	if err := secureDir(dir, 0700); err != nil {
		return err
	}
	if state.RepoRoot != "" {
		relative, err := filepath.Rel(state.RepoRoot, dir)
		if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return errors.New("保存先は納品先 repo の外にしてください")
		}
	}
	if secrets != nil {
		if err := writeEnv(filepath.Join(dir, "runtime.env"), secrets); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "init.json"), append(data, '\n'), 0600)
}

func secureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("保存先はシンボリックリンクにできません")
	}
	return os.Chmod(dir, mode)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("保存先は通常ファイルである必要があります")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".init-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(name, path)
}

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func writeEnv(path string, values Secrets) error {
	keys := make([]string, 0, len(values))
	for k, v := range values {
		if !envName.MatchString(k) || strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("env の名前または値が不正です")
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&out, "%s=%s\n", k, values[k])
	}
	return atomicWrite(path, []byte(out.String()), 0600)
}

func (s *State) Redo(stage string) error {
	index := -1
	for i, name := range stages {
		if name == stage {
			index = i
		}
	}
	if index < 0 {
		return fmt.Errorf("--redo は %s のいずれかです", strings.Join(stages, ", "))
	}
	for _, name := range stages[index:] {
		delete(s.Completed, name)
		delete(s.Checks, name)
	}
	// Keep external IDs and smoke correlation: redo must never duplicate resources.
	return nil
}

func randomHex(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
func fingerprint(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func newRunID() (string, error) {
	suffix, err := randomHex(12)
	return "run_" + time.Now().UTC().Format("20060102") + "_" + suffix, err
}
