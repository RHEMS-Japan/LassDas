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
//	                                 3 when it can, 4 when PATH is missing,
//	                                 2 when the launcher cannot switch users
package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// orphanCheckInterval is how often a running launcher looks whether the
// engine that started it is still its parent.
var orphanCheckInterval = 2 * time.Second

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
		default:
			return invocation{}, fmt.Errorf("unknown argument %q", arg)
		}
		if err != nil {
			return invocation{}, err
		}
	}
	modes := 0
	for _, set := range []bool{inv.reclaim != "", inv.check != "", len(inv.command) > 0} {
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
	if root := os.Getenv(treeRootEnv); root != "" {
		// The blast radius of a chown: only trees under the runs directory
		// are lent or returned, whatever path a caller names.
		for _, dir := range []string{inv.workspace, inv.home, inv.reclaim} {
			if dir != "" && !within(root, dir) {
				return invocation{}, fmt.Errorf("%s is outside %s, the only tree this launcher lends or returns", dir, root)
			}
		}
	}
	return inv, nil
}

// treeRootEnv names the directory under which the launcher lends and
// returns trees; set by the entrypoint to the runs directory.
const treeRootEnv = "LASSDAS_AGENT_TREE_ROOT"

// within reports whether dir is root or under it, by cleaned absolute paths
// (symlinks are not followed anywhere in this program).
func within(root, dir string) bool {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	return absDir == absRoot || strings.HasPrefix(absDir, absRoot+string(filepath.Separator))
}

// launch lends the workspace to the agent user, runs the command as that
// user and returns the workspace afterwards.
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
	if err := lendTree(inv.workspace, inv.uid, inv.gid); err != nil {
		fmt.Fprintln(stderr, "agentexec: workspace:", err)
		if back := chownTree(inv.workspace, uint32(os.Getuid()), uint32(os.Getgid())); back != nil {
			fmt.Fprintln(stderr, "agentexec: workspace not returned:", back)
		}
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
	// The agent user's processes are out of the engine's reach (a signal
	// from one user does not reach another's), so stopping is this
	// launcher's job: the agent and its tools form their own process
	// group, which the launcher kills (cap_kill) when the engine asks it
	// to stop, and the kernel kills the agent when this launcher dies for
	// any reason (the parent-death signal, sent with the launcher's
	// capabilities). The thread that starts the child is the one whose
	// death that signal follows, so it is pinned for the launcher's life.
	runtime.LockOSThread()
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: inv.uid, Gid: inv.gid, Groups: []uint32{inv.gid}},
		Setpgid:    true,
	}
	dieWithParent(command.SysProcAttr)
	if err := command.Start(); err != nil {
		if errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "operation not permitted") {
			fmt.Fprintln(stderr, "agentexec: cannot switch to the agent user: the launcher lacks its capabilities")
		} else {
			fmt.Fprintln(stderr, "agentexec:", err)
		}
		return exitLauncher
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(stop)
	// The engine that started this launcher may die without a word (a
	// card's wall kills the engine's process group, which this launcher is
	// not in); an orphaned launcher stops its agent rather than leaving it
	// running with its keys.
	parent := os.Getppid()
	orphanWatch := time.NewTicker(orphanCheckInterval)
	defer orphanWatch.Stop()
	reason := ""
	for reason == "" {
		select {
		case err = <-done:
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					return exit.ExitCode()
				}
				fmt.Fprintln(stderr, "agentexec:", err)
				return exitLauncher
			}
			return 0
		case received := <-stop:
			reason = received.String()
		case <-orphanWatch.C:
			if os.Getppid() != parent {
				reason = "the engine that started this launcher is gone"
			}
		}
	}
	fmt.Fprintf(stderr, "agentexec: stopping the agent (%s)\n", reason)
	if killErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); killErr != nil {
		fmt.Fprintln(stderr, "agentexec: the agent's process group could not be signalled:", killErr)
	}
	<-done
	return exitLauncher
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
	// The whole home: the agent's program keeps its state under it (a
	// Hermes profile makes directories of its own beside its config).
	return lendTree(home, uid, gid)
}

// lendTree gives every entry under root to the agent user, deepest first:
// a directory is changed after its contents were listed, because a
// directory the agent user owns and others cannot read would end the walk
// (the launcher holds no cap_dac_read_search). Symlinks are changed, not
// followed.
func lendTree(root string, uid, gid uint32) error {
	// The top of a lent tree is closed to everyone but its user: agents
	// are different users of one group, and one must not read another's
	// workspace or home. Closed while this user still owns it (a chmod
	// needs the owner), and it stays closed after the return (its owner,
	// the engine, reads it all the same).
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	paths, err := treeDeepestFirst(root)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := os.Lchown(path, int(uid), int(gid)); err != nil {
			return err
		}
	}
	return nil
}

// treeDeepestFirst lists root and everything under it, every directory
// after its contents.
func treeDeepestFirst(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(paths)-1; i < j; i, j = i+1, j-1 {
		paths[i], paths[j] = paths[j], paths[i]
	}
	return paths, nil
}

// chownTree returns every entry under root to the given user, a directory
// before its contents, so a directory the agent user closed opens again
// for the listing. Symlinks are changed, not followed.
func chownTree(root string, uid, gid uint32) error {
	// Best effort to the end: a tree half returned is worse than one
	// returned with its failures listed.
	var failures []error
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			failures = append(failures, err)
			return nil
		}
		if err := os.Lchown(path, int(uid), int(gid)); err != nil {
			failures = append(failures, err)
		}
		return nil
	})
	if walkErr != nil {
		failures = append(failures, walkErr)
	}
	return errors.Join(failures...)
}

// check asks a child running as the agent user to open the path; the
// child's exit says what the agent user can do. The child is the system
// shell, not this program: the launcher is executable by the engine's
// user alone, so the agent user could not run a probe built into it. A
// control run first proves the switch and the shell work, so a failing
// probe means "closed" and never "could not look". The children get a
// bare environment: the engine's variables are not theirs to see.
func check(inv invocation, stderr io.Writer) int {
	if _, err := os.Lstat(inv.check); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return exitMissing
		}
		fmt.Fprintln(stderr, "agentexec:", err)
		return exitLauncher
	}
	credential := &syscall.Credential{Uid: inv.uid, Gid: inv.gid, Groups: []uint32{inv.gid}}
	control := exec.Command("/bin/sh", "-c", "exit 0")
	control.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	control.Env = []string{"PATH=/usr/bin:/bin"}
	if err := control.Run(); err != nil {
		fmt.Fprintln(stderr, "agentexec: cannot switch to the agent user: the launcher lacks its capabilities")
		return exitLauncher
	}
	probe := exec.Command("/bin/sh", "-c", `exec 3<"$1"`, "agentexec-probe", inv.check)
	probe.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	probe.Env = []string{"PATH=/usr/bin:/bin"}
	err := probe.Run()
	if err == nil {
		return exitReadable
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return 0
	}
	fmt.Fprintln(stderr, "agentexec:", err)
	return exitLauncher
}
