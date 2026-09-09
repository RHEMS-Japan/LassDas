// Package localrun runs one generated instance through the existing runtime
// image. It owns no daemon, release mechanism, or host source checkout.
package localrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Instance is saved by init. Image must include a distribution digest, and
// EngineSHA must come from the corresponding build record, not an image tag.
type Instance struct {
	ID            string `json:"id"`
	Dir           string `json:"dir"`
	Image         string `json:"image"`
	EngineSHA     string `json:"engine_sha"`
	DockerContext string `json:"docker_context"`
	BoardPort     int    `json:"board_port"`
}

type Status struct {
	State       string `json:"state"` // missing, stopped, starting, ready
	ContainerID string `json:"container_id,omitempty"`
	BoardURL    string `json:"board_url"`
}

// Docker receives arguments, never a shell command. Its output may contain
// credentials and must not be included in errors or diagnostic messages.
type Docker interface {
	Run(context.Context, []string) ([]byte, error)
}

type Manager struct {
	Docker       Docker
	HTTPClient   *http.Client
	StartTimeout time.Duration
	PollInterval time.Duration
}

type dockerCLI struct{}

func (dockerCLI) Run(ctx context.Context, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).CombinedOutput()
}

func (m Manager) command(ctx context.Context, i Instance, args ...string) ([]byte, error) {
	docker := m.Docker
	if docker == nil {
		docker = dockerCLI{}
	}
	name := i.DockerContext
	if name == "" {
		name = "desktop-linux"
	}
	out, err := docker.Run(ctx, append([]string{"--context", name}, args...))
	if err != nil {
		// Neither Docker output nor an implementation's error is safe to
		// print: entrypoint failures and logs can contain environment values.
		return nil, fmt.Errorf("docker %s failed (inspect the instance logs)", args[0])
	}
	return out, nil
}

func identity(i Instance) string {
	digest := sha256.Sum256([]byte(filepath.Clean(i.Dir) + "\x00" + i.ID))
	return hex.EncodeToString(digest[:])
}

func resourceName(i Instance) string { return "lassdas-" + i.ID + "-" + identity(i)[:12] }

const ownerLabel = "org.lassdas.local-instance"
const fingerprintLabel = "org.lassdas.local-config"

type container struct {
	ID     string
	Image  string
	Labels map[string]string
	State  struct {
		Running   bool
		Status    string
		StartedAt time.Time
	}
}

// inspect lists before inspecting so a daemon failure is never mistaken for
// absence. Exact names avoid touching another project's container.
func (m Manager) inspect(ctx context.Context, i Instance) (*container, error) {
	out, err := m.command(ctx, i, "container", "ls", "--all", "--format", "{{.Names}}", "--filter", "name=^/"+resourceName(i)+"$")
	if err != nil {
		return nil, err
	}
	found := false
	for _, name := range strings.Fields(string(out)) {
		found = found || name == resourceName(i)
	}
	if !found {
		return nil, nil
	}
	out, err = m.command(ctx, i, "container", "inspect", "--format", `{"ID":{{json .Id}},"Image":{{json .Config.Image}},"Labels":{{json .Config.Labels}},"State":{{json .State}}}`, resourceName(i))
	if err != nil {
		return nil, err
	}
	var c container
	if json.Unmarshal(out, &c) != nil || c.ID == "" {
		return nil, errors.New("Docker returned an invalid container description")
	}
	if c.Labels[ownerLabel] != identity(i) {
		return nil, errors.New("container name belongs to a different instance; no changes made")
	}
	return &c, nil
}

func statusOf(i Instance, c *container) Status {
	s := Status{State: "missing", BoardURL: fmt.Sprintf("http://127.0.0.1:%d", i.BoardPort)}
	if c != nil {
		s.ContainerID = c.ID
		s.State = "stopped"
		if c.State.Running {
			s.State = "starting"
		}
	}
	return s
}

// Start is idempotent for unchanged configuration. Changed configuration
// requires stopping the instance first; only its stopped container is replaced.
func (m Manager) Start(ctx context.Context, i Instance) (Status, error) {
	p, err := prepare(i)
	if err != nil {
		return Status{}, err
	}
	i = p.instance
	c, err := m.inspect(ctx, i)
	if err != nil {
		return Status{}, err
	}
	if c != nil && c.State.Running {
		if c.Labels[fingerprintLabel] != p.fingerprint || c.Image != i.Image {
			return statusOf(i, c), errors.New("configuration changed; stop this instance before starting it again")
		}
		return m.awaitReady(ctx, p, c, false)
	}
	if err := m.checkImage(ctx, p); err != nil {
		return statusOf(i, c), err
	}
	if err := m.prepareVolume(ctx, p); err != nil {
		return statusOf(i, c), err
	}
	args := []string{"run", "--rm", "--pull=never", "--network=none", "--user", "1000:1000", "--entrypoint", "/bin/sh"}
	args = append(args, p.mounts()...)
	args = append(args, "--env", "LASSDAS_AGENT_TREE_ROOT=/data/runs", i.Image, "-ec", preflightScript)
	if _, err := m.command(ctx, i, args...); err != nil {
		return statusOf(i, c), errors.New("runtime preflight failed: check config pins, volume ownership, and agent separation")
	}
	args = []string{"run", "--rm", "--pull=never", "--network=none", "--user", "1000:1000", "--entrypoint", "/usr/local/bin/worker", "--env-file", filepath.Join(i.Dir, "runtime.env")}
	args = append(args, p.mounts()...)
	args = append(args, i.Image, "check-runtime", "--config", "/etc/lassdas/config/runtime.json")
	if _, err := m.command(ctx, i, args...); err != nil {
		return statusOf(i, c), errors.New("runtime Load/BuildServices preflight failed; verify configuration and that the pinned image includes check-runtime")
	}
	if c != nil && (c.Labels[fingerprintLabel] != p.fingerprint || c.Image != i.Image) {
		if _, err := m.command(ctx, i, "container", "rm", c.ID); err != nil {
			return statusOf(i, c), err
		}
		c = nil
	}
	if c == nil {
		args = []string{"create", "--pull=never", "--name", resourceName(i), "--label", ownerLabel + "=" + identity(i), "--label", fingerprintLabel + "=" + p.fingerprint,
			"--user", "1000:1000", "--publish", fmt.Sprintf("127.0.0.1:%d:9200", i.BoardPort), "--env-file", filepath.Join(i.Dir, "runtime.env")}
		args = append(args, p.mounts()...)
		args = append(args, i.Image)
		if _, err := m.command(ctx, i, args...); err != nil {
			return Status{}, err
		}
	}
	if _, err := m.command(ctx, i, "container", "start", resourceName(i)); err != nil {
		return Status{}, err
	}
	c, err = m.inspect(ctx, i)
	if err != nil {
		return Status{}, err
	}
	return m.awaitReady(ctx, p, c, true)
}

// Stop never removes state or a volume. It remains usable when configuration
// or credentials are broken, so an incomplete init cannot prevent stop-loss.
func (m Manager) Stop(ctx context.Context, i Instance) error {
	if err := validateIdentity(i); err != nil {
		return err
	}
	c, err := m.inspect(ctx, i)
	if err != nil || c == nil || !c.State.Running {
		return err
	}
	_, err = m.command(ctx, i, "container", "stop", "--time", "30", c.ID)
	return err
}

// Status reports ready only after current boot, secret, resident and HTTP
// checks. Running alone does not mean the receptionist has started.
func (m Manager) Status(ctx context.Context, i Instance) (Status, error) {
	if err := validateIdentity(i); err != nil {
		return Status{}, err
	}
	c, err := m.inspect(ctx, i)
	s := statusOf(i, c)
	if err != nil || c == nil || !c.State.Running {
		return s, err
	}
	p, err := prepare(i)
	if err != nil {
		return s, err
	}
	if c.Image != i.Image || c.Labels[fingerprintLabel] != p.fingerprint {
		return s, errors.New("running configuration differs from the saved instance; stop before applying changes")
	}
	if err := m.ready(ctx, p, c); err != nil {
		return s, err
	}
	s.State = "ready"
	return s, nil
}

// Logs prints a bounded tail and redacts the configured credentials. The env
// file is never emitted. Logs refuse to run without it, rather than printing
// potentially unredacted historical credentials after a failed read.
func (m Manager) Logs(ctx context.Context, i Instance, dst io.Writer) error {
	if err := validateIdentity(i); err != nil {
		return err
	}
	p, err := prepare(i)
	if err != nil {
		return err
	}
	c, err := m.inspect(ctx, i)
	if err != nil {
		return err
	}
	if c == nil {
		return errors.New("instance container does not exist")
	}
	if c.Labels[fingerprintLabel] != p.fingerprint {
		return errors.New("saved configuration changed; logs require the original runtime.env to redact that container's credentials")
	}
	out, err := m.command(ctx, i, "container", "logs", "--tail", "200", c.ID)
	if err != nil {
		return err
	}
	log := string(out)
	var secrets []string
	for key, value := range p.env {
		if sensitive(key) && value != "" {
			secrets = append(secrets, value)
		}
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, value := range secrets {
		log = strings.ReplaceAll(log, value, "[redacted]")
	}
	_, err = io.WriteString(dst, log)
	return err
}

func (m Manager) awaitReady(ctx context.Context, p prepared, c *container, started bool) (Status, error) {
	wait := m.StartTimeout
	if wait <= 0 {
		wait = 90 * time.Second
	}
	interval := m.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	last := errors.New("runtime has not started")
	for checkCtx.Err() == nil {
		if c == nil || !c.State.Running {
			last = errors.New("runtime exited before readiness; inspect the instance logs")
			break
		}
		if err := m.ready(checkCtx, p, c); err == nil {
			s := statusOf(p.instance, c)
			s.State = "ready"
			return s, nil
		} else {
			last = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-checkCtx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if checkCtx.Err() != nil {
			break
		}
		var err error
		c, err = m.inspect(checkCtx, p.instance)
		if err != nil {
			last = err
			break
		}
	}
	if started {
		// A failed startup is not left dispatching in the background. Stop
		// gets its own short deadline even when the caller canceled init.
		stopCtx, stop := context.WithTimeout(context.Background(), 35*time.Second)
		defer stop()
		if err := m.Stop(stopCtx, p.instance); err != nil {
			return statusOf(p.instance, c), fmt.Errorf("startup incomplete: %v; could not stop the instance: %w", last, err)
		}
	}
	s := statusOf(p.instance, c)
	if started && c != nil {
		s.State = "stopped"
	}
	return s, fmt.Errorf("startup incomplete: %w", last)
}

func (m Manager) ready(ctx context.Context, p prepared, c *container) error {
	_, err := m.command(ctx, p.instance, "exec", "--user", "1000:1000", c.ID, "python3", "-c", readyScript, c.State.StartedAt.Format(time.RFC3339Nano))
	if err != nil {
		return errors.New("boot checks incomplete: residents, fresh board snapshot, required secrets, and agent restrictions must all pass")
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if m.HTTPClient != nil {
		copy := *m.HTTPClient
		copy.CheckRedirect = client.CheckRedirect
		if copy.Timeout == 0 {
			copy.Timeout = client.Timeout
		}
		client = &copy
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", p.instance.BoardPort)
	checks := []struct {
		path string
		auth bool
		want int
	}{{"/healthz", false, http.StatusOK}, {"/", false, http.StatusUnauthorized}, {"/", true, http.StatusOK}}
	if p.env["LASSDAS_BOARD_AUTH"] == "local" {
		checks[1].want = http.StatusOK
		checks[2].path = "/api/board"
		checks[2].auth = false
	}
	for _, check := range checks {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+check.path, nil)
		if err != nil {
			return errors.New("could not prepare board check")
		}
		if check.auth {
			req.SetBasicAuth(p.env["LASSDAS_BOARD_USER"], p.env["LASSDAS_BOARD_PASS"])
		}
		response, err := client.Do(req)
		if err != nil {
			return errors.New("board is not reachable on the saved loopback port")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode != check.want {
			return fmt.Errorf("board %s authentication check failed (HTTP %d)", check.path, response.StatusCode)
		}
	}
	return nil
}
