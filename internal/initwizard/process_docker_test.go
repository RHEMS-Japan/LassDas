package initwizard

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Exercise the real clone scripts and Git ownership checks without credentials
// or network access. Set the image to a locally available runtime digest.
func TestConsumerCloneOwnershipDocker(t *testing.T) {
	image := os.Getenv("LASSDAS_INIT_TEST_IMAGE")
	if image == "" {
		t.Skip("set LASSDAS_INIT_TEST_IMAGE to run the Docker ownership regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	const fixture = `set -eu
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
git -c init.templateDir= init /fixture >/dev/null
printf 'fixture\n' >/fixture/README.md
git -C /fixture add README.md
git -C /fixture -c user.name=Fixture -c user.email=fixture@example.com commit -qm fixture
set -- example/cli "$(git -C /fixture rev-parse HEAD)"
printf 'artificial-token' >/auth/token
chmod 700 /auth
chmod 600 /auth/token
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=url.file:///fixture.insteadOf
export GIT_CONFIG_VALUE_0=https://github.com/example/cli.git
`
	const verify = `
test "$(stat -c %u /work)" = 1000
test "$(stat -c %u /work/repo)" = 1000
test "$(stat -c %u /work/repo/.git)" = 1000
su -s /bin/sh lassdas -c '
set -eu
test "$(id -u)" = 1000
git -C /work/repo status --porcelain
test "$(cat /work/repo/README.md)" = fixture
test ! -r /auth/token
printf "{}\n" >/work/check.json
'
test -s /work/check.json
`
	script := fixture + prepareConsumerClone + "\n" + cloneConsumer + verify
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never",
		"--network=none", "--user=0", "--tmpfs", "/work:rw,nosuid,nodev",
		"--tmpfs", "/auth:rw,nosuid,nodev", "--entrypoint", "/bin/sh", image, "-c", script)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone and credential-free verifier failed: %v\n%s", err, output)
	}
}
