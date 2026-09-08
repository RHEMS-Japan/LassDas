package localrun

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

func (p prepared) mounts() []string {
	return []string{"--mount", "type=volume,src=" + resourceName(p.instance) + "-data,dst=/data",
		"--mount", "type=bind,src=" + filepath.Join(p.instance.Dir, "config") + ",dst=/etc/lassdas/config,readonly"}
}

func (m Manager) checkImage(ctx context.Context, p prepared) error {
	out, err := m.command(ctx, p.instance, "info", "--format", "{{.OSType}}|{{.Architecture}}|{{.OperatingSystem}}")
	if err != nil {
		return err
	}
	info := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(info) != 3 || info[0] != "linux" || (info[1] != "aarch64" && info[1] != "arm64") || !strings.Contains(info[2], "Docker Desktop") {
		return errors.New("local runtime requires Docker Desktop with linux/arm64; no fallback is applied")
	}
	inspect := func() ([]byte, error) {
		return m.command(ctx, p.instance, "image", "inspect", "--format", `{"OS":{{json .Os}},"Arch":{{json .Architecture}},"User":{{json .Config.User}},"Digests":{{json .RepoDigests}},"Entrypoint":{{json .Config.Entrypoint}}}`, p.instance.Image)
	}
	out, err = inspect()
	if err != nil {
		if _, err := m.command(ctx, p.instance, "pull", "--platform", "linux/arm64", p.instance.Image); err != nil {
			return errors.New("pinned runtime image is unavailable; acquire registry access outside init and retry")
		}
		out, err = inspect()
		if err != nil {
			return err
		}
	}
	var image struct {
		OS, Arch, User string
		Digests        []string
		Entrypoint     []string
	}
	if json.Unmarshal(out, &image) != nil || image.OS != "linux" || image.Arch != "arm64" || (image.User != "lassdas" && image.User != "1000" && image.User != "1000:1000") ||
		len(image.Entrypoint) != 1 || image.Entrypoint[0] != "/usr/local/bin/lassdas-entrypoint" {
		return errors.New("image must be the arm64 runtime with uid 1000 and the established entrypoint")
	}
	for _, digest := range image.Digests {
		if digest == p.instance.Image {
			return nil
		}
	}
	return errors.New("image distribution digest does not match the saved instance")
}

func (m Manager) prepareVolume(ctx context.Context, p prepared) error {
	name := resourceName(p.instance) + "-data"
	out, err := m.command(ctx, p.instance, "volume", "ls", "--format", "{{.Name}}", "--filter", "name=^"+name+"$")
	if err != nil {
		return err
	}
	found := false
	for _, value := range strings.Fields(string(out)) {
		found = found || value == name
	}
	if !found {
		if _, err := m.command(ctx, p.instance, "volume", "create", "--label", ownerLabel+"="+identity(p.instance), name); err != nil {
			return err
		}
	}
	out, err = m.command(ctx, p.instance, "volume", "inspect", "--format", `{{json .Labels}}`, name)
	var labels map[string]string
	if err != nil || json.Unmarshal(out, &labels) != nil || labels[ownerLabel] != identity(p.instance) {
		return errors.New("data volume is not owned by this instance; no ownership changes made")
	}
	// Also recovers interruption between creating and preparing an empty
	// volume. There is no recursive chown of an existing state directory.
	_, err = m.command(ctx, p.instance, "run", "--rm", "--pull=never", "--network=none", "--user", "0:0", "--entrypoint", "/bin/sh",
		"--mount", "type=volume,src="+name+",dst=/data", p.instance.Image, "-ec", volumeScript)
	if err != nil {
		return errors.New("data volume is not writable by uid 1000; existing data ownership was preserved")
	}
	return nil
}

const volumeScript = `
if [ "$(stat -c '%u:%g' /data)" = '1000:1000' ]; then exit 0; fi
test -z "$(find /data -mindepth 1 -maxdepth 1 -print -quit)"
chown 1000:1000 /data
chmod 0755 /data
`

// This container has no env file and no network. Its disposable workspace is
// under runs so it exercises the same agentexec boundary as actual runs.
const preflightScript = `
test "$(id -u)" = 1000
test -r /etc/lassdas/config/runtime.json
test -r /etc/lassdas/config/consumer.json
test ! -e /etc/lassdas/runtime.env
test -r /etc/lassdas/tool-pins.txt
cd /usr/local/bin
sha256sum -c /etc/lassdas/tool-pins.txt >/dev/null
python3 -c 'import json,hashlib; c=json.load(open("/etc/lassdas/config/runtime.json")); assert all(hashlib.sha256(open(c[k+"_bin"],"rb").read()).hexdigest()==c[k+"_sha256"] for k in ["worker","controller"])'
mkdir -p /data/runs /data/instance /data/secrets
test -w /data
CHECK_DIR=$(mktemp -d /data/runs/.localrun-check-XXXXXXXX)
cleanup() {
  agentexec --reclaim "$CHECK_DIR/workspace" >/dev/null 2>&1 || true
  agentexec --reclaim "$CHECK_DIR/home" >/dev/null 2>&1 || true
  rm -rf "$CHECK_DIR"
}
trap cleanup EXIT
chmod 0711 "$CHECK_DIR"
mkdir "$CHECK_DIR/workspace" "$CHECK_DIR/home"
printf 'artificial-check' > "$CHECK_DIR/secret"
chmod 0600 "$CHECK_DIR/secret"
agentexec --check "$CHECK_DIR/secret" >/dev/null
agentexec --uid 2001 --gid 2000 --workspace "$CHECK_DIR/workspace" --home "$CHECK_DIR/home" -- /bin/sh -ec 'test "$(id -u)" = 2001; test "$(id -g)" = 2000; test ! -r "$1"; test -r /etc/passwd' sh "$CHECK_DIR/secret"
`

// Readiness uses the resident's snapshot and heartbeat from this boot, not a
// log line. The snapshot is written only after runtime.Load and BuildServices.
// It reads only local state and probes the launcher's reserved probe uid.
const readyScript = `
import datetime, glob, json, os, pathlib, stat, subprocess, sys, time
assert os.getuid() == 1000
started = datetime.datetime.fromisoformat(sys.argv[1].replace('Z', '+00:00')).timestamp()
config = json.load(open('/etc/lassdas/config/runtime.json'))
assert os.environ['HERMES_KANBAN_BOARD'] == config['hermes_board']
assert os.environ['HERMES_KANBAN_DB'] == '/data/kanban.db'
assert os.environ['LASSDAS_AGENT_TREE_ROOT'] == config['chain']['runs_root']
for path in ['/data/secrets/target-token', '/data/secrets/board-pass', '/data/route.key'] + (['/data/secrets/board-tracker-key'] if os.environ.get('LASSDAS_BOARD_TRACKER_KEY') else []):
    st = os.lstat(path)
    assert stat.S_ISREG(st.st_mode) and st.st_uid == 1000 and st.st_mode & 0o077 == 0 and st.st_size > 0
    with open(path, 'rb') as secret: assert secret.read(1)
    assert subprocess.run(['/usr/local/bin/agentexec', '--check', path], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
for path in ['/data/heartbeat', '/data/status/board.json']:
    stamp = os.stat(path).st_mtime
    assert stamp >= started and time.time() - stamp < 130
snapshot = json.load(open('/data/status/board.json'))
assert datetime.datetime.fromisoformat(snapshot['generated_at'].replace('Z', '+00:00')).timestamp() >= started
resident = False
for path in glob.glob('/proc/[0-9]*/cmdline'):
    try:
        argv = pathlib.Path(path).read_bytes().split(b'\0')
        if argv and os.path.basename(argv[0]) == b'attendant' and b'--config' in argv and b'/etc/lassdas/config/runtime.json' in argv:
            resident = True
    except (FileNotFoundError, PermissionError): pass
assert resident
`
