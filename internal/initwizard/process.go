package initwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/worker"
)

// Process receives argument vectors, never shell-joined user input. Output from
// failed commands is deliberately excluded: tools may echo remote credentials.
type Process interface {
	Run(context.Context, string, []string, string) ([]byte, error)
	LookPath(string) (string, error)
}
type ExecProcess struct{}

func (ExecProcess) LookPath(name string) (string, error) { return exec.LookPath(name) }
func (ExecProcess) Run(ctx context.Context, name string, args []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// The host may hold unrelated production/model keys. Subprocesses only get
	// the ordinary tool lookup and Docker Desktop client configuration.
	for _, key := range []string{"PATH", "HOME", "DOCKER_CONFIG", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "LANG"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s の実行に失敗しました (出力は秘密保護のため非表示)", name)
	}
	return output, nil
}

func (w *Wizard) docker(ctx context.Context, s *State, args ...string) ([]byte, error) {
	if s.DockerContext != "" {
		args = append([]string{"--context", s.DockerContext}, args...)
	}
	return w.Process.Run(ctx, "docker", args, "")
}

var imagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
var pinPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (w *Wizard) imageCheck(ctx context.Context, s *State) error {
	if !imagePattern.MatchString(s.Image) || !worker.ValidToolSHA(s.EngineSHA) || s.BuildRecord == "" {
		return errors.New("image は digest 固定、ソースは 40 桁 SHA、対応を確認したビルド記録が必要です")
	}
	if s.DockerContext == "" {
		raw, err := w.Process.Run(ctx, "docker", []string{"context", "show"}, "")
		if err != nil {
			return err
		}
		s.DockerContext = strings.TrimSpace(string(raw))
	}
	info, err := w.docker(ctx, s, "info", "--format", "{{.OSType}}/{{.Architecture}}")
	if err != nil {
		return err
	}
	platform := strings.TrimSpace(string(info))
	if platform != "linux/aarch64" && platform != "linux/arm64" {
		return errors.New("初版は Docker Desktop の linux/arm64 を対象とします")
	}
	start := time.Now()
	_, err = w.docker(ctx, s, "pull", "--platform", "linux/arm64", s.Image)
	s.Metrics.DownloadSeconds += time.Since(start).Seconds()
	if err != nil {
		return errors.New("固定 image を取得できません。非公開 image の認証は外で済ませてください")
	}
	raw, err := w.docker(ctx, s, "image", "inspect", "--format", "{{.Os}}/{{.Architecture}}", s.Image)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(raw)) != "linux/arm64" {
		return errors.New("取得 image は linux/arm64 ではありません")
	}
	const pinsScript = `set -eu
cd /usr/local/bin
sha256sum -c /etc/lassdas/tool-pins.txt >/dev/null
cat /etc/lassdas/tool-pins.txt`
	raw, err = w.docker(ctx, s, "run", "--rm", "--network", "none", "--entrypoint", "/bin/sh", s.Image, "-c", pinsScript)
	if err != nil {
		return errors.New("image の tool-pins と実バイナリが一致しません")
	}
	pins := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		name := filepath.Base(strings.TrimPrefix(parts[1], "*"))
		if pinPattern.MatchString(parts[0]) {
			pins[name] = parts[0]
		}
	}
	for _, name := range []string{"worker", "controller", "browsercheck"} {
		if pins[name] == "" {
			return fmt.Errorf("image に %s の pin がありません", name)
		}
	}
	s.Pins = pins
	return nil
}

// Keep the fresh clone root-owned until Git finishes its ownership checks.
// The credential-free verifier receives the repository and output directory last.
const prepareConsumerClone = `set -eu
mkdir /work/repo`

const cloneConsumer = `set -eu
printf '#!/bin/sh\ncase "$1" in *Username*) printf "x-access-token\\n";; *) cat /auth/token;; esac\n' >/tmp/git-askpass
chmod 700 /tmp/git-askpass
export GIT_ASKPASS=/tmp/git-askpass GIT_TERMINAL_PROMPT=0
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
git -c init.templateDir= init /work/repo >/dev/null
git -C /work/repo -c credential.helper= -c core.hooksPath=/dev/null fetch --depth=1 "https://github.com/$1.git" "$2" >/dev/null 2>&1
git -C /work/repo -c core.hooksPath=/dev/null checkout --detach "$2" >/dev/null 2>&1
test "$(git -C /work/repo rev-parse HEAD)" = "$2"
chown -R 1000:1000 /work/repo
chown 1000:1000 /work`

// checkConsumer separates cloning (the delivery key only) from verification
// (no credentials). Only a fresh disposable named volume joins those steps.
func (w *Wizard) checkConsumer(ctx context.Context, s *State, secrets Secrets, dir string) error {
	suffix, err := randomHex(12)
	if err != nil {
		return err
	}
	volume := "ticket-init-check-" + suffix
	if _, err = w.docker(ctx, s, "volume", "create", volume); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = w.docker(cleanup, s, "volume", "rm", volume)
	}()
	temp, err := os.MkdirTemp(dir, ".consumer-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	// Nonsecret config is readable by image uid 1000; its host parent stays private.
	if err = os.Chmod(temp, 0755); err != nil {
		return err
	}
	raw, err := marshal(Consumer(s))
	if err != nil {
		return err
	}
	if err = atomicWrite(filepath.Join(temp, "consumer.json"), raw, 0644); err != nil {
		return err
	}
	authDir, err := os.MkdirTemp(dir, ".clone-auth-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(authDir)
	if err = atomicWrite(filepath.Join(authDir, "token"), []byte(secrets["TARGET_GITHUB_TOKEN"]), 0600); err != nil {
		return err
	}
	if _, err = w.docker(ctx, s, "run", "--rm", "--network", "none", "--user", "0", "--mount", "type=volume,src="+volume+",dst=/work", "--entrypoint", "/bin/sh", s.Image, "-c", prepareConsumerClone); err != nil {
		return err
	}
	// Clone runs as root solely to read the host's 0600 token. No repository
	// code executes here; credentials are not available in the following check.
	if _, err = w.docker(ctx, s, "run", "--rm", "--user", "0", "--mount", "type=volume,src="+volume+",dst=/work", "--mount", "type=bind,src="+authDir+",dst=/auth,readonly", "--entrypoint", "/bin/sh", s.Image, "-c", cloneConsumer, "clone", s.Repository, s.BaseSHA); err != nil {
		return errors.New("確定 SHA の clone に失敗しました。納品用 GitHub 鍵の read 権限と枝を確認してください")
	}
	if err = os.RemoveAll(authDir); err != nil {
		return err
	}
	_, err = w.docker(ctx, s, "run", "--rm", "--user", "1000:1000", "--mount", "type=volume,src="+volume+",dst=/work", "--mount", "type=bind,src="+temp+",dst=/check,readonly", "--entrypoint", "/usr/local/bin/worker", s.Image, "check-consumer", "--consumer", "/check/consumer.json", "--repo-root", "/work/repo", "--base-sha", s.BaseSHA, "--out", "/work/check.json")
	if err != nil {
		return errors.New("image 内の納品先検査に失敗しました。道具・検証コマンド・検証中の管理ファイル変更を確認してください")
	}
	raw, err = w.docker(ctx, s, "run", "--rm", "--network", "none", "--user", "1000:1000", "--mount", "type=volume,src="+volume+",dst=/work,readonly", "--entrypoint", "/bin/cat", s.Image, "/work/check.json")
	if err != nil {
		return err
	}
	if !json.Valid(raw) {
		return errors.New("納品先検査の記録を読み取れません")
	}
	s.Checks["consumer"] = append(json.RawMessage(nil), raw...)
	return nil
}

func (w *Wizard) checkRuntime(ctx context.Context, s *State, dir string) error {
	// This invokes Load + BuildServices offline with disposable storage. The
	// config validator needs a nonempty bot key, not the real runtime secrets.
	_, err := w.docker(ctx, s, "run", "--rm", "--network", "none", "--env", "BACKLOG_API_KEY=init-offline-validation", "--tmpfs", "/data:uid=1000,gid=1000,mode=0700", "--mount", "type=bind,src="+filepath.Join(dir, "config")+",dst=/etc/lassdas/config,readonly", "--entrypoint", "/usr/local/bin/worker", s.Image, "check-runtime", "--config", "/etc/lassdas/config/runtime.json")
	return err
}

func marshal(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	return append(raw, '\n'), err
}
