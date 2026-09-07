// agentexec runs an agent process as the agent user on behalf of the engine
// user (docs/RUNTIME_POD.md, "Agents under their own user"). The engine runs
// unprivileged; this launcher is the one program in the image installed
// with file capabilities (cap_setuid, cap_setgid, cap_chown) and executable
// by the engine's user alone. It lends the agent the workspace for the run
// (chown to the agent user, chown back when the agent exits), starts the
// agent under the agent user with the environment it was handed (HOME
// pointing at the agent's own home) and passes the exit code through. It
// never widens what the agent can reach beyond the workspace: the kept
// session jar, the seed mount, the engine's secrets and the sealed run
// records stay closed to the agent user by their modes.
//
//	agentexec [--uid N] [--gid N] --workspace DIR --home DIR -- COMMAND [ARGS...]
//	agentexec --reclaim DIR          returns a workspace to the engine user
//	agentexec --check PATH           exit 0 when the agent user cannot open PATH,
//	                                 3 when it can, 2 when the launcher cannot switch users
//	agentexec --probe PATH           (internal) the child of --check
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	defaultAgentUID = 2000
	defaultAgentGID = 2000
	// exitLauncher says the launcher itself could not do its job (no
	// capability, bad arguments); exitReadable is --check finding the path
	// open to the agent user.
	exitLauncher = 2
	exitReadable = 3
	exitMissing  = 4
)

type invocation struct {
	uid, gid  uint32
	workspace string
	home      string
	reclaim   string
	check     string
	probe     string
	command   []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	inv, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, "agentexec:", err)
		return exitLauncher
	}
	switch {
	case inv.probe != "":
		return probe(inv.probe)
	case inv.check != "":
		return check(inv, stderr)
	case inv.reclaim != "":
		if err := chownTree(inv.reclaim, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
			fmt.Fprintln(stderr, "agentexec: reclaim:", err)
			return exitLauncher
		}
		return 0
	}
	return launch(inv, stdout, stderr)
}

func parse(args []string) (invocation, error) {
	inv := invocation{uid: defaultAgentUID, gid: defaultAgentGID}
	if value := os.Getenv("LASSDAS_AGENT_UID"); value != "" {
		id, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return invocation{}, errors.New("LASSDAS_AGENT_UID is not a number")
		}
		inv.uid = uint32(id)
	}
	if value := os.Getenv("LASSDAS_AGENT_GID"); value != "" {
		id, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return invocation{}, errors.New("LASSDAS_AGENT_GID is not a number")
		}
		inv.gid = uint32(id)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", arg)
			}
			i++
			return args[i], nil
		}
		var err error
		switch arg {
		case "--":
			inv.command = args[i+1:]
			i = len(args)
		case "--uid":
			var raw string
			if raw, err = value(); err == nil {
				var id uint64
				if id, err = strconv.ParseUint(raw, 10, 32); err == nil {
					inv.uid = uint32(id)
				}
			}
		case "--gid":
			var raw string
			if raw, err = value(); err == nil {
				var id uint64
				if id, err = strconv.ParseUint(raw, 10, 32); err == nil {
					inv.gid = uint32(id)
				}
			}
		case "--workspace":
			inv.workspace, err = value()
		case "--home":
			inv.home, err = value()
		case "--reclaim":
			inv.reclaim, err = value()
		case "--check":
			inv.check, err = value()
		case "--probe":
			inv.probe, err = value()
		default:
			return invocation{}, fmt.Errorf("unknown argument %q", arg)
		}
		if err != nil {
			return invocation{}, err
		}
	}
	modes := 0
	for _, set := range []bool{inv.reclaim != "", inv.check != "", inv.probe != "", len(inv.command) > 0} {
		if set {
			modes++
		}
	}
	if modes != 1 {
		return invocation{}, errors.New("give exactly one of: -- COMMAND, --reclaim DIR, --check PATH")
	}
	if len(inv.command) > 0 && (inv.workspace == "" || inv.home == "") {
		return invocation{}, errors.New("--workspace and --home are required to run a command")
	}
	if inv.uid == uint32(os.Getuid()) || inv.uid == 0 {
		return invocation{}, fmt.Errorf("the agent user %d would not be a separate user", inv.uid)
	}
	return inv, nil
}

// launch lends the workspace to the agent user, runs the command as that
// user and returns the workspace afterwards. The command shares this
// process's group, so the engine's group signal reaches the agent too.
func launch(inv invocation, stdout, stderr io.Writer) int {
	info, err := os.Stat(inv.workspace)
	if err != nil || !info.IsDir() {
		fmt.Fprintln(stderr, "agentexec: workspace is not a directory")
		return exitLauncher
	}
	if err := ensureHome(inv.home, inv.uid, inv.gid); err != nil {
		fmt.Fprintln(stderr, "agentexec: agent home:", err)
		return exitLauncher
	}
	if err := chownTree(inv.workspace, inv.uid, inv.gid); err != nil {
		fmt.Fprintln(stderr, "agentexec: workspace:", err)
		return exitLauncher
	}
	defer func() {
		if err := chownTree(inv.workspace, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
			fmt.Fprintln(stderr, "agentexec: workspace not returned:", err)
		}
	}()
	command := exec.Command(inv.command[0], inv.command[1:]...) // #nosec G204 -- the engine's validated launch definition.
	command.Dir = inv.workspace
	command.Env = agentEnv(os.Environ(), inv.home)
	command.Stdin = nil
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: inv.uid, Gid: inv.gid, Groups: []uint32{inv.gid}}}
	if err := command.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted") {
			fmt.Fprintln(stderr, "agentexec: cannot switch to the agent user: the launcher lacks its capabilities")
		} else {
			fmt.Fprintln(stderr, "agentexec:", err)
		}
		return exitLauncher
	}
	return 0
}

// agentEnv is the handed environment with the agent's own home; the
// engine's home and user names must not leak into the agent's view.
func agentEnv(environ []string, home string) []string {
	out := make([]string, 0, len(environ)+3)
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "HOME", "USER", "LOGNAME":
			continue
		}
		out = append(out, entry)
	}
	return append(out, "HOME="+home, "USER=agent", "LOGNAME=agent")
}

// ensureHome gives the agent user its home and the top of its .hermes
// tree; the profiles inside stay the engine's, readable and rewritten by
// it on every boot.
func ensureHome(home string, uid, gid uint32) error {
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return errors.New("not a directory")
	}
	for _, dir := range []string{home, filepath.Join(home, ".hermes")} {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			if err := os.Lchown(dir, int(uid), int(gid)); err != nil {
				return err
			}
		}
	}
	return nil
}

// chownTree changes the owner of every entry under root without following
// symbolic links, so a link inside a workspace cannot reach outside it.
func chownTree(root string, uid, gid uint32) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		return os.Lchown(path, int(uid), int(gid))
	})
}

// check asks a child running as the agent user to open the path; the
// child's exit says what the agent user can do.
func check(inv invocation, stderr io.Writer) int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, "agentexec:", err)
		return exitLauncher
	}
	command := exec.Command(self, "--probe", inv.check) // #nosec G204 -- this program itself.
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: inv.uid, Gid: inv.gid, Groups: []uint32{inv.gid}}}
	err = command.Run()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	fmt.Fprintln(stderr, "agentexec: cannot switch to the agent user: the launcher lacks its capabilities")
	return exitLauncher
}

// probe is the child of check: it runs as the agent user and reports
// whether the path opens.
func probe(path string) int {
	file, err := os.Open(path)
	if err == nil {
		_ = file.Close()
		return exitReadable
	}
	if errors.Is(err, fs.ErrPermission) {
		return 0
	}
	if errors.Is(err, fs.ErrNotExist) {
		return exitMissing
	}
	return 0
}
