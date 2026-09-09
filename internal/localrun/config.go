package localrun

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"automation.internal/ticket-ingress/internal/runtime"
)

var instanceID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
var imageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var sourceSHA = regexp.MustCompile(`^[a-f0-9]{40}$`)
var contextName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

type prepared struct {
	instance    Instance
	env         map[string]string
	fingerprint string
}

func validateIdentity(i Instance) error {
	if !instanceID.MatchString(i.ID) || !filepath.IsAbs(i.Dir) || strings.ContainsAny(i.Dir, ",\r\n\x00") {
		return errors.New("instance needs a lowercase project id and an absolute directory without commas or control characters")
	}
	if i.DockerContext != "" && !contextName.MatchString(i.DockerContext) {
		return errors.New("invalid Docker context name")
	}
	return nil
}

func checkFile(path string, mode os.FileMode, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != mode || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return fmt.Errorf("%s must be a real %s with mode %04o", filepath.Base(path), map[bool]string{true: "directory", false: "file"}[dir], mode)
	}
	return nil
}

func prepare(i Instance) (prepared, error) {
	if err := validateIdentity(i); err != nil {
		return prepared{}, err
	}
	if !imageRef.MatchString(i.Image) || !sourceSHA.MatchString(i.EngineSHA) {
		return prepared{}, errors.New("the runtime image needs a pinned sha256 digest and its 40-hex source commit from the build record")
	}
	if i.BoardPort < 1024 || i.BoardPort > 65535 {
		return prepared{}, errors.New("board port must be between 1024 and 65535")
	}
	if err := checkFile(i.Dir, 0o700, true); err != nil {
		return prepared{}, err
	}
	configDir := filepath.Join(i.Dir, "config")
	if err := checkFile(configDir, 0o755, true); err != nil {
		return prepared{}, err
	}
	entries, err := os.ReadDir(configDir)
	if err != nil || len(entries) != 2 {
		return prepared{}, errors.New("config directory must contain only runtime.json and m1-consumer.json")
	}
	hash := sha256.New()
	for _, name := range []string{"runtime.json", "m1-consumer.json"} {
		path := filepath.Join(configDir, name)
		if err := checkFile(path, 0o644, false); err != nil {
			return prepared{}, err
		}
		content, err := os.ReadFile(path)
		if err != nil || !json.Valid(content) {
			return prepared{}, fmt.Errorf("%s must contain valid JSON", name)
		}
		_, _ = hash.Write(content)
		_, _ = hash.Write([]byte{0})
	}
	rawConfig, err := os.ReadFile(filepath.Join(configDir, "runtime.json"))
	if err != nil {
		return prepared{}, errors.New("runtime.json could not be read")
	}
	decoder := json.NewDecoder(strings.NewReader(string(rawConfig)))
	decoder.DisallowUnknownFields()
	var config runtime.Config
	if err := decoder.Decode(&config); err != nil {
		// Input may contain accidentally pasted credentials. Do not echo
		// parser messages that include the invalid value.
		return prepared{}, errors.New("runtime.json has invalid or unknown fields")
	}
	// Runtime.Load also loads consumer_config_path, which intentionally names
	// a container path. Full validation runs through check-runtime in the
	// pinned image; host inspection checks only the local launch contract.
	if config.Identity.EngineSHA != i.EngineSHA || config.LedgerPath != "/data/ledger.db" || config.ConsumerConfigPath != "/etc/lassdas/config/m1-consumer.json" ||
		config.KnowledgeRoot != "/data/instance" || config.Chain.RunsRoot != "/data/runs" || config.Chain.TargetTokenPath != "/data/secrets/target-token" ||
		config.Orchestration != "cards" || config.HermesBoard == "" || config.HermesProfile != "lassdas-runner" || config.HermesBin != "/usr/local/bin/hermes" ||
		config.WorkerBin != "/usr/local/bin/worker" || config.ControllerBin != "/usr/local/bin/controller" || config.BrowserCheckBin != "" ||
		config.WorkerSHA256 == "" || config.ControllerSHA256 == "" || config.Chain.E2EProfile != "" || config.Chain.Deliver != (runtime.DeliverConfig{}) {
		return prepared{}, errors.New("runtime.json does not match the saved image source or the local cards runtime paths and PR-only contract")
	}
	profiles, _ := json.Marshal(config.Chain.Profiles)
	var byStage map[string]string
	_ = json.Unmarshal(profiles, &byStage)
	for _, stage := range []string{"implementer", "review_a", "review_b", "validate", "publish", "investigate", "design_review_a", "design_review_b", "design_decide", "applier"} {
		if byStage[stage] != "lassdas-"+strings.ReplaceAll(stage, "_", "-") {
			return prepared{}, errors.New("runtime chain profiles must match the image's ten stage profiles")
		}
	}
	env, envBytes, err := readEnvironment(i.Dir)
	if err != nil {
		return prepared{}, err
	}
	wantEnv := map[string]string{
		"LASSDAS_RUNTIME_CONFIG": "/etc/lassdas/config/runtime.json", "LASSDAS_STATE_DIR": "/data",
		"HERMES_KANBAN_DB": "/data/kanban.db", "LASSDAS_AGENT_TREE_ROOT": "/data/runs", "HERMES_KANBAN_BOARD": config.HermesBoard,
		"LASSDAS_GUARDED_FILES": "/data/secrets/target-token:/data/secrets/board-pass:/data/secrets/board-tracker-key:/data/route.key",
	}
	for key, value := range wantEnv {
		if env[key] != value {
			return prepared{}, fmt.Errorf("runtime.env %s must match the local runtime configuration", key)
		}
	}
	for _, key := range []string{"TARGET_GITHUB_TOKEN", "BACKLOG_API_KEY", "LASSDAS_GATEWAY_BASE_URL", "LASSDAS_IMPLEMENTER_KEY", "LASSDAS_REVIEW_A_KEY", "LASSDAS_REVIEW_B_KEY", "LASSDAS_DESIGNER_KEY", "LASSDAS_APPLIER_KEY", "LASSDAS_INTAKE_TARGET_KEY", "LASSDAS_READINESS_ASSESSOR_KEY", "LASSDAS_READINESS_CHECKER_KEY"} {
		if strings.TrimSpace(env[key]) == "" {
			return prepared{}, fmt.Errorf("runtime.env %s is required", key)
		}
	}
	switch env["LASSDAS_BOARD_AUTH"] {
	case "", "basic", "local":
	default:
		return prepared{}, errors.New("board authentication mode must be basic or local")
	}
	if env["LASSDAS_BOARD_AUTH"] != "local" && (strings.TrimSpace(env["LASSDAS_BOARD_USER"]) == "" || strings.Contains(env["LASSDAS_BOARD_USER"], ":") || len(env["LASSDAS_BOARD_PASS"]) < 16 || strings.TrimSpace(env["LASSDAS_BOARD_PASS"]) != env["LASSDAS_BOARD_PASS"]) {
		return prepared{}, errors.New("board user must not contain a colon and board password must have at least 16 characters without surrounding whitespace")
	}
	trio := 0
	for _, key := range []string{"LASSDAS_BOARD_TRACKER_KEY", "LASSDAS_BOARD_TRACKER_ORIGIN", "LASSDAS_BOARD_TRACKER_SPACE"} {
		if env[key] != "" {
			trio++
		}
	}
	if trio != 0 && trio != 3 {
		return prepared{}, errors.New("optional board tracker settings must be provided together")
	}
	if env["LASSDAS_BOARD_AUTH"] == "local" && trio != 0 {
		return prepared{}, errors.New("the local board is read-only and does not accept tracker action credentials")
	}
	for _, letter := range []string{"A", "B"} {
		keyVar := "LASSDAS_DESIGN_REVIEW_" + letter + "_KEY_VAR"
		if value := env[keyVar]; value != "" && value != "LASSDAS_REVIEW_"+letter+"_KEY" && value != "LASSDAS_DESIGN_REVIEW_"+letter+"_KEY" {
			return prepared{}, fmt.Errorf("runtime.env %s must name that reviewer's key", keyVar)
		}
		if value := env[keyVar]; value != "" && env[value] == "" {
			return prepared{}, fmt.Errorf("runtime.env %s names a missing key", keyVar)
		}
	}
	_, _ = hash.Write(envBytes)
	_, _ = fmt.Fprintf(hash, "\x00%s\x00%s\x00%d", i.Image, i.EngineSHA, i.BoardPort)
	return prepared{instance: i, env: env, fingerprint: hex.EncodeToString(hash.Sum(nil))}, nil
}

func readEnvironment(dir string) (map[string]string, []byte, error) {
	path := filepath.Join(dir, "runtime.env")
	if err := checkFile(path, 0o600, false); err != nil {
		return nil, nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, errors.New("runtime.env could not be read")
	}
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !allowedEnvironment(key) || strings.ContainsAny(value, "\r\x00") {
			return nil, nil, errors.New("runtime.env contains an unsupported name or invalid line; values are literal KEY=value entries")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, nil, errors.New("runtime.env contains a duplicate variable")
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return nil, nil, errors.New("runtime.env contains an oversized line")
	}
	return values, raw, nil
}

func sensitive(key string) bool {
	return strings.HasSuffix(key, "_KEY") || strings.HasSuffix(key, "_TOKEN") || strings.HasSuffix(key, "_PASS")
}

func allowedEnvironment(key string) bool {
	switch key {
	case "LASSDAS_RUNTIME_CONFIG", "LASSDAS_STATE_DIR", "HERMES_KANBAN_DB", "HERMES_KANBAN_BOARD", "LASSDAS_AGENT_TREE_ROOT", "LASSDAS_GUARDED_FILES",
		"TARGET_GITHUB_TOKEN", "BACKLOG_API_KEY", "LASSDAS_GATEWAY_BASE_URL", "LASSDAS_BOARD_AUTH", "LASSDAS_BOARD_USER", "LASSDAS_BOARD_PASS",
		"LASSDAS_BOARD_TRACKER_KEY", "LASSDAS_BOARD_TRACKER_ORIGIN", "LASSDAS_BOARD_TRACKER_SPACE", "LASSDAS_DESIGN_REVIEW_A_KEY_VAR", "LASSDAS_DESIGN_REVIEW_B_KEY_VAR":
		return true
	}
	for _, role := range []string{"IMPLEMENTER", "REVIEW_A", "REVIEW_B", "DESIGNER", "APPLIER", "DESIGN_REVIEW_A", "DESIGN_REVIEW_B", "INTAKE_TARGET", "READINESS_ASSESSOR", "READINESS_CHECKER"} {
		for _, suffix := range []string{"KEY", "MODEL"} {
			if key == "LASSDAS_"+role+"_"+suffix {
				return true
			}
		}
	}
	return false
}
