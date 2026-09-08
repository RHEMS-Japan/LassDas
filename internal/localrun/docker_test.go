package localrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test uses an already-cached distribution digest, artificial
// data, no network and a disposable named volume. It never pulls or starts
// normal residents. No credentials or source checkout enter the container.
func TestDockerIsolationAndRestart(t *testing.T) {
	image := os.Getenv("LASSDAS_LOCALRUN_TEST_IMAGE")
	if image == "" {
		t.Skip("set LASSDAS_LOCALRUN_TEST_IMAGE to a cached runtime distribution digest")
	}
	if !imageRef.MatchString(image) {
		t.Fatal("test image must be a distribution digest")
	}
	t.Setenv("TMPDIR", "/tmp")
	i := fixture(t)
	i.Image = image
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	docker := dockerCLI{}
	call := func(args ...string) []byte {
		t.Helper()
		out, err := docker.Run(ctx, append([]string{"--context", "desktop-linux"}, args...))
		if err != nil {
			t.Fatalf("disposable Docker %s failed: %s", args[0], out)
		}
		return out
	}
	call("image", "inspect", "--format", "{{.Architecture}}", image)
	pins := call("run", "--rm", "--pull=never", "--network=none", "--entrypoint", "/bin/cat", image, "/etc/lassdas/tool-pins.txt")
	runtimePath := filepath.Join(i.Dir, "config", "runtime.json")
	raw, _ := os.ReadFile(runtimePath)
	var config map[string]any
	_ = json.Unmarshal(raw, &config)
	for _, line := range strings.Split(string(pins), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			config[fields[1]+"_sha256"] = fields[0]
		}
	}
	// The observation binary is checked by the image pin manifest but remains
	// disabled in the runtime config.
	delete(config, "browsercheck_sha256")
	raw, _ = json.Marshal(config)
	writeFile(t, runtimePath, raw, 0o644)
	p, err := prepare(i)
	if err != nil {
		t.Fatal(err)
	}
	m := Manager{}
	volume := resourceName(i) + "-data"
	// The random temporary directory makes this test's resource identity
	// unique. Cleanup names that exact volume and no other local instance.
	if err := m.prepareVolume(ctx, p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		if _, err := docker.Run(cleanupCtx, []string{"--context", "desktop-linux", "volume", "rm", volume}); err != nil {
			t.Error("disposable test volume cleanup failed")
		}
	})
	run := func(script string, user string) []byte {
		t.Helper()
		args := []string{"run", "--rm", "--pull=never", "--network=none", "--user", user, "--entrypoint", "/bin/sh"}
		args = append(args, p.mounts()...)
		args = append(args, "--env", "LASSDAS_AGENT_TREE_ROOT=/data/runs", image, "-ec", script)
		return call(args...)
	}
	run(preflightScript, "1000:1000")
	run(`
python3 - <<'PY'
import errno, json, os, sqlite3
assert os.getuid() == 1000
assert not os.path.exists('/etc/lassdas/runtime.env')
try:
    open('/etc/lassdas/config/runtime.json', 'a')
    raise AssertionError('config mount was writable')
except OSError as e:
    assert e.errno in (errno.EROFS, errno.EACCES)
db=sqlite3.connect('/data/localrun-test.db')
assert db.execute('pragma journal_mode=wal').fetchone()[0] == 'wal'
db.execute('create table persisted(value text)')
db.execute("insert into persisted values ('kept')")
db.commit()
db.close()
PY
`, "1000:1000")
	if err := m.prepareVolume(ctx, p); err != nil {
		t.Fatal(err)
	}
	run(`python3 -c "import sqlite3; db=sqlite3.connect('/data/localrun-test.db'); assert db.execute('select value from persisted').fetchone()[0]=='kept'; db.close()"`, "1000:1000")
	run(`printf artificial-secret > /data/secrets/leaky; chmod 0644 /data/secrets/leaky`, "0:0")
	run(`test -r /data/secrets/leaky; if agentexec --check /data/secrets/leaky; then exit 1; else test "$?" = 3; fi`, "1000:1000")
	run(`printf artificial-secret > /data/secrets/correctable; chmod 0644 /data/secrets/correctable; chmod go-rwx /data/secrets/correctable; agentexec --check /data/secrets/correctable`, "1000:1000")
}
