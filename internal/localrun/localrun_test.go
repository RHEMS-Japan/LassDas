package localrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Instance {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	i := Instance{ID: "example", Dir: dir, Image: "registry.example/runtime@sha256:" + strings.Repeat("a", 64), EngineSHA: strings.Repeat("b", 40), BoardPort: 9200}
	config := map[string]any{
		"ledger_path": "/data/ledger.db", "consumer_config_path": "/etc/lassdas/config/m1-consumer.json", "knowledge_root": "/data/instance",
		"worker_bin": "/usr/local/bin/worker", "controller_bin": "/usr/local/bin/controller", "hermes_bin": "/usr/local/bin/hermes",
		"worker_sha256": strings.Repeat("c", 64), "controller_sha256": strings.Repeat("d", 64),
		"hermes_board": "project-example", "hermes_profile": "lassdas-runner", "orchestration": "cards",
		"automation_run_id":   "run_20260908_" + strings.Repeat("ab", 12),
		"identity":            map[string]any{"repository_id": 1, "repository": "example/runtime", "workflow_ref": "example/runtime/local-runtime@" + i.EngineSHA, "engine_sha": i.EngineSHA},
		"tracker":             map[string]any{"origin": "https://example.backlog.com", "space_key": "example", "project_id": 1, "project_key": "EXAMPLE", "allowed_creator_id": 7, "allowed_activity_type": 1},
		"report_destinations": []any{map[string]any{"repository": "example/consumer", "delivery": "pull_request"}},
	}
	profiles := map[string]string{}
	for _, stage := range []string{"implementer", "review_a", "review_b", "validate", "publish", "investigate", "design_review_a", "design_review_b", "design_decide", "applier"} {
		profiles[stage] = "lassdas-" + strings.ReplaceAll(stage, "_", "-")
	}
	config["chain"] = map[string]any{"runs_root": "/data/runs", "target_token_path": "/data/secrets/target-token", "profiles": profiles}
	raw, _ := json.Marshal(config)
	writeFile(t, filepath.Join(dir, "config", "runtime.json"), raw, 0o644)
	writeFile(t, filepath.Join(dir, "config", "m1-consumer.json"), []byte(`{"consumers":[]}`), 0o644)
	env := "LASSDAS_RUNTIME_CONFIG=/etc/lassdas/config/runtime.json\nLASSDAS_STATE_DIR=/data\nHERMES_KANBAN_DB=/data/kanban.db\nHERMES_KANBAN_BOARD=project-example\nLASSDAS_AGENT_TREE_ROOT=/data/runs\nLASSDAS_GUARDED_FILES=/data/secrets/target-token:/data/secrets/board-pass:/data/secrets/board-tracker-key:/data/route.key\nTARGET_GITHUB_TOKEN=artificial-target-token\nBACKLOG_API_KEY=artificial-tracker-key\nLASSDAS_GATEWAY_BASE_URL=https://models.example/v1\nLASSDAS_BOARD_USER=viewer\nLASSDAS_BOARD_PASS=artificial-board-password\n"
	for _, role := range []string{"IMPLEMENTER", "REVIEW_A", "REVIEW_B", "DESIGNER", "APPLIER", "INTAKE_TARGET", "READINESS_ASSESSOR", "READINESS_CHECKER"} {
		env += "LASSDAS_" + role + "_KEY=artificial-" + role + "-key\n"
	}
	writeFile(t, filepath.Join(dir, "runtime.env"), []byte(env), 0o600)
	return i
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

type fakeDocker struct {
	t           *testing.T
	i           Instance
	c           *container
	volume      bool
	volumeOwner string
	commands    [][]string
	fail        string
	logs        string
}

func (d *fakeDocker) Run(_ context.Context, original []string) ([]byte, error) {
	d.t.Helper()
	d.commands = append(d.commands, slices.Clone(original))
	if len(original) < 3 || original[0] != "--context" || original[1] != "desktop-linux" {
		d.t.Fatalf("wrong context: %v", original)
	}
	args := original[2:]
	if d.fail != "" && (args[0] == d.fail || slices.Contains(args, d.fail)) {
		return []byte("artificial-target-token"), errors.New("artificial-target-token")
	}
	marshal := func(value any) ([]byte, error) { return json.Marshal(value) }
	switch args[0] {
	case "info":
		return []byte("linux|aarch64|Docker Desktop"), nil
	case "image":
		return marshal(map[string]any{"OS": "linux", "Arch": "arm64", "User": "lassdas", "Digests": []string{d.i.Image}, "Entrypoint": []string{"/usr/local/bin/lassdas-entrypoint"}})
	case "volume":
		switch args[1] {
		case "ls":
			if d.volume {
				return []byte(resourceName(d.i) + "-data\n"), nil
			}
			return nil, nil
		case "create":
			d.volume, d.volumeOwner = true, identity(d.i)
			return []byte(resourceName(d.i) + "-data"), nil
		case "inspect":
			return marshal(map[string]string{ownerLabel: d.volumeOwner})
		}
	case "run", "exec":
		return nil, nil
	case "create":
		if d.c != nil {
			return nil, errors.New("name exists")
		}
		d.c = &container{ID: "container-test", Image: d.i.Image, Labels: map[string]string{}}
		for index, arg := range args {
			if arg == "--label" {
				key, value, _ := strings.Cut(args[index+1], "=")
				d.c.Labels[key] = value
			}
		}
		return []byte(d.c.ID), nil
	case "container":
		switch args[1] {
		case "ls":
			if d.c != nil {
				return []byte(resourceName(d.i) + "\n"), nil
			}
			return nil, nil
		case "inspect":
			return marshal(d.c)
		case "start":
			d.c.State.Running = true
			d.c.State.StartedAt = time.Now().UTC()
			return nil, nil
		case "stop":
			d.c.State.Running = false
			return nil, nil
		case "rm":
			if d.c.State.Running || slices.Contains(args, "--force") || slices.Contains(args, "-f") {
				d.t.Fatal("unsafe container removal")
			}
			d.c = nil
			return nil, nil
		case "logs":
			return []byte(d.logs), nil
		}
	}
	d.t.Fatalf("unexpected Docker command: %v", args)
	return nil, nil
}

type boardTransport struct {
	wants []string
	open  bool
}

func (b *boardTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	user, pass, auth := req.BasicAuth()
	b.wants = append(b.wants, fmt.Sprintf("%s:%t", req.URL.Path, auth))
	code := http.StatusUnauthorized
	if req.URL.Path == "/healthz" || b.open || (user == "viewer" && pass == "artificial-board-password") {
		code = http.StatusOK
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader("response")), Header: make(http.Header), Request: req}, nil
}

func managerFor(t *testing.T, i Instance) (Manager, *fakeDocker, *boardTransport) {
	t.Helper()
	d := &fakeDocker{t: t, i: i}
	b := &boardTransport{}
	return Manager{Docker: d, HTTPClient: &http.Client{Transport: b}, StartTimeout: 5 * time.Millisecond, PollInterval: time.Millisecond}, d, b
}

func countCommand(d *fakeDocker, name string) int {
	n := 0
	for _, args := range d.commands {
		if args[2] == name {
			n++
		}
	}
	return n
}

func TestStartChecksIsolationAndAuthenticationAndIsIdempotent(t *testing.T) {
	i := fixture(t)
	m, d, board := managerFor(t, i)
	for range 2 {
		s, err := m.Start(context.Background(), i)
		if err != nil || s.State != "ready" || s.BoardURL != "http://127.0.0.1:9200" {
			t.Fatalf("Start = %+v, %v", s, err)
		}
	}
	if countCommand(d, "create") != 1 || countCommand(d, "run") != 3 {
		t.Fatalf("idempotent start created extra resources: %+v", d.commands)
	}
	if !slices.Equal(board.wants[:3], []string{"/healthz:false", "/:false", "/:true"}) {
		t.Fatalf("wrong board checks: %v", board.wants)
	}
	for _, args := range d.commands {
		joined := strings.Join(args, " ")
		for _, forbidden := range []string{"artificial-target-token", "--privileged", "seccomp=unconfined", "docker.sock", "--cap-add", "--net=host"} {
			if strings.Contains(joined, forbidden) {
				t.Fatalf("unsafe argument %s", forbidden)
			}
		}
		if args[2] == "create" && (!slices.Contains(args, "127.0.0.1:9200:9200") || !slices.Contains(args, "type=bind,src="+filepath.Join(i.Dir, "config")+",dst=/etc/lassdas/config,readonly")) {
			t.Fatal("runtime must expose only loopback and mount only readonly config")
		}
		if args[2] == "run" && (!slices.Contains(args, "--network=none") || !slices.Contains(args, "--entrypoint")) {
			t.Fatal("preflight must not reach network or boot normal residents")
		}
		if slices.Contains(args, "0:0") && slices.Contains(args, "--env-file") {
			t.Fatal("volume bootstrap received secrets")
		}
	}
}

func TestLocalBoardStartsWithoutCredentialsAndRejectsAuthenticationPrompt(t *testing.T) {
	for _, open := range []bool{true, false} {
		t.Run(fmt.Sprintf("open=%t", open), func(t *testing.T) {
			i := fixture(t)
			path := filepath.Join(i.Dir, "runtime.env")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := []string{"LASSDAS_BOARD_AUTH=local"}
			for _, line := range strings.Split(string(raw), "\n") {
				if !strings.HasPrefix(line, "LASSDAS_BOARD_USER=") && !strings.HasPrefix(line, "LASSDAS_BOARD_PASS=") {
					lines = append(lines, line)
				}
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
				t.Fatal(err)
			}
			m, d, board := managerFor(t, i)
			board.open = open
			s, err := m.Start(context.Background(), i)
			if open {
				if err != nil || s.State != "ready" || !slices.Equal(board.wants, []string{"/healthz:false", "/:false", "/api/board:false"}) {
					t.Fatalf("local board startup = %+v, %v, checks %v", s, err, board.wants)
				}
			} else if err == nil || s.State != "stopped" || d.c.State.Running {
				t.Fatalf("board requiring authentication was accepted: %+v, %v", s, err)
			}
		})
	}
}

func TestRestartRetainsVolumeAndChangedRunningConfigRequiresStop(t *testing.T) {
	i := fixture(t)
	m, d, _ := managerFor(t, i)
	if _, err := m.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(i.Dir, "runtime.env")
	raw, _ := os.ReadFile(path)
	writeFile(t, path, append(raw, []byte("LASSDAS_IMPLEMENTER_MODEL=example/model\n")...), 0o600)
	if _, err := m.Start(context.Background(), i); err == nil || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("changed running config should require stop: %v", err)
	}
	if err := m.Stop(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if countCommand(d, "create") != 2 || !d.volume {
		t.Fatal("changed stopped container must be recreated while retaining its volume")
	}
	for _, args := range d.commands {
		if slices.Contains(args, "prune") || (args[2] == "volume" && args[3] == "rm") {
			t.Fatal("state volume was destroyed")
		}
	}
}

func TestRefusesUnownedResourcesAndDaemonFailure(t *testing.T) {
	for _, mode := range []string{"container", "volume", "daemon"} {
		t.Run(mode, func(t *testing.T) {
			i := fixture(t)
			m, d, _ := managerFor(t, i)
			switch mode {
			case "container":
				d.c = &container{ID: "other", Labels: map[string]string{ownerLabel: "other"}}
			case "volume":
				d.volume, d.volumeOwner = true, "other"
			case "daemon":
				d.fail = "container"
			}
			if _, err := m.Start(context.Background(), i); err == nil {
				t.Fatal("unsafe start succeeded")
			}
			if countCommand(d, "run") != 0 || countCommand(d, "create") != 0 {
				t.Fatal("mutated unowned resources or treated failure as absence")
			}
		})
	}
}

func TestFailedReadinessStopsStartedContainerWithoutLosingState(t *testing.T) {
	for _, failure := range []string{"guard", "board"} {
		t.Run(failure, func(t *testing.T) {
			i := fixture(t)
			m, d, board := managerFor(t, i)
			if failure == "guard" {
				d.fail = "exec"
			} else {
				board.open = true
			}
			s, err := m.Start(context.Background(), i)
			if err == nil || s.State != "stopped" || d.c.State.Running || !d.volume {
				t.Fatalf("failed startup = %+v, %v", s, err)
			}
			if strings.Contains(err.Error(), "artificial-target-token") {
				t.Fatal("command error exposed credential")
			}
		})
	}
}

func TestStopWorksWithMissingEnvironmentAndIsIdempotent(t *testing.T) {
	i := fixture(t)
	m, d, _ := managerFor(t, i)
	if _, err := m.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(i.Dir, "runtime.env")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.Stop(context.Background(), i); err != nil {
			t.Fatal(err)
		}
	}
	if d.c.State.Running || !d.volume {
		t.Fatal("stop did not preserve stopped instance state")
	}
}

func TestInvalidInputsDoNotInvokeDocker(t *testing.T) {
	for _, failure := range []string{"tag", "source", "port", "env-mode", "extra-config", "symlink", "board-mismatch", "env-override", "secret-missing"} {
		t.Run(failure, func(t *testing.T) {
			i := fixture(t)
			envPath := filepath.Join(i.Dir, "runtime.env")
			raw, _ := os.ReadFile(envPath)
			switch failure {
			case "tag":
				i.Image = "registry.example/runtime:latest"
			case "source":
				i.EngineSHA = strings.Repeat("e", 40)
			case "port":
				i.BoardPort = 80
			case "env-mode":
				_ = os.Chmod(envPath, 0o644)
			case "extra-config":
				writeFile(t, filepath.Join(i.Dir, "config", "secret"), []byte("artificial"), 0o600)
			case "symlink":
				_ = os.Rename(envPath, envPath+".real")
				_ = os.Symlink(envPath+".real", envPath)
			case "board-mismatch":
				writeFile(t, envPath, bytes.ReplaceAll(raw, []byte("BOARD=project-example"), []byte("BOARD=other")), 0o600)
			case "env-override":
				writeFile(t, envPath, append(raw, []byte("LASSDAS_AGENT_LAUNCHER=/bin/true\n")...), 0o600)
			case "secret-missing":
				writeFile(t, envPath, bytes.ReplaceAll(raw, []byte("TARGET_GITHUB_TOKEN=artificial-target-token"), []byte("TARGET_GITHUB_TOKEN=")), 0o600)
			}
			m, d, _ := managerFor(t, i)
			if _, err := m.Start(context.Background(), i); err == nil {
				t.Fatal("invalid input accepted")
			}
			if len(d.commands) != 0 {
				t.Fatal("Docker invoked for invalid input")
			}
		})
	}
}

func TestLogsRedactCredentialsAndRefuseUnreadableEnv(t *testing.T) {
	i := fixture(t)
	m, d, _ := managerFor(t, i)
	if _, err := m.Start(context.Background(), i); err != nil {
		t.Fatal(err)
	}
	d.logs = "test artificial-target-token artificial-tracker-key artificial-board-password"
	var out bytes.Buffer
	if err := m.Logs(context.Background(), i, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "test [redacted] [redacted] [redacted]" {
		t.Fatalf("unredacted logs: %q", out.String())
	}
	_ = os.Chmod(filepath.Join(i.Dir, "runtime.env"), 0o644)
	out.Reset()
	if err := m.Logs(context.Background(), i, &out); err == nil || out.Len() != 0 {
		t.Fatal("logs emitted without safe credential read")
	}
}
