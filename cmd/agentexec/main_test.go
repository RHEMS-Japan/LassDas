package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The launcher does exactly one thing per call, names the agent user it
// would switch to, and refuses to run anything as the caller itself.
func TestParseInsistsOnOneModeAndASeparateUser(t *testing.T) {
	inv, err := parse([]string{"--uid", "2001", "--workspace", "/w", "--home", "/h", "--", "hermes", "--profile", "x"})
	if err != nil || inv.workspace != "/w" || inv.home != "/h" || strings.Join(inv.command, " ") != "hermes --profile x" || inv.uid != defaultAgentUID+1 {
		t.Fatalf("parse = %+v, %v", inv, err)
	}
	for name, args := range map[string][]string{
		"no mode":              {"--workspace", "/w", "--home", "/h"},
		"two modes":            {"--reclaim", "/w", "--check", "/x"},
		"command without home": {"--workspace", "/w", "--", "hermes"},
		"unknown flag":         {"--bogus", "1", "--", "hermes"},
		"same user":            {"--uid", "0", "--workspace", "/w", "--home", "/h", "--", "hermes"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("%s: accepted %v", name, args)
		}
	}
	self := []string{"--uid", strconv.Itoa(os.Getuid()), "--workspace", "/w", "--home", "/h", "--", "hermes"}
	if _, err := parse(self); err == nil {
		t.Fatal("running the agent as the caller's own user was accepted")
	}
}

// The agent sees its own home and user names, never the engine's.
func TestAgentEnvReplacesTheHome(t *testing.T) {
	env := agentEnv([]string{"HOME=/home/engine", "USER=engine", "PATH=/bin", "MODEL_KEY=k"}, "/data/agent-home", 2000)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/home/engine") || strings.Contains(joined, "USER=engine") || !strings.Contains(joined, "HOME=/data/agent-home") || !strings.Contains(joined, "USER=agent") || !strings.Contains(joined, "MODEL_KEY=k") {
		t.Fatalf("env = %q", env)
	}
}

// Without its capabilities the launcher fails closed and says so; it never
// runs the agent as the caller.
func TestRunWithoutCapabilitiesFailsClosed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can switch users; the closed failure needs an unprivileged caller")
	}
	workspace := t.TempDir()
	home := t.TempDir()
	var out, errs bytes.Buffer
	code := run([]string{"--workspace", workspace, "--home", home, "--", "true"}, &out, &errs)
	if code != exitLauncher || !strings.Contains(errs.String(), "agentexec:") {
		t.Fatalf("code = %d, stderr = %q; want the launcher to refuse", code, errs.String())
	}
	if code := run([]string{"--check", workspace}, &out, &errs); code != exitLauncher {
		t.Fatalf("--check without capabilities = %d, want %d", code, exitLauncher)
	}
}

// Lending walks deepest first: a closed directory (0700) handed to the
// agent user before its contents were listed would end the walk, and the
// workspace would be half lent.
func TestLendingOrdersADirectoryAfterItsContents(t *testing.T) {
	root := t.TempDir()
	closed := filepath.Join(root, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(closed, "file")
	if err := os.WriteFile(inner, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := treeDeepestFirst(root)
	if err != nil {
		t.Fatal(err)
	}
	index := func(want string) int {
		for i, path := range paths {
			if path == want {
				return i
			}
		}
		t.Fatalf("%s not listed", want)
		return -1
	}
	if !(index(inner) < index(closed) && index(closed) < index(root)) {
		t.Fatalf("order = %v, want the file, then its directory, then the root", paths)
	}
	// Lending to this same user is a no-op chown that must still walk the
	// closed directory.
	if err := lendTree(root, uint32(os.Getuid()), uint32(os.Getgid())); err != nil {
		t.Fatalf("lendTree: %v", err)
	}
}

// With a tree root set, the launcher lends and returns nothing outside it,
// whatever path it is given.
func TestParseKeepsToTheTreeRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv(treeRootEnv, root)
	inside := filepath.Join(root, "delivery_1", "target-repo")
	if _, err := parse([]string{"--reclaim", inside}); err != nil {
		t.Fatalf("a path under the root was refused: %v", err)
	}
	for _, outside := range []string{"/usr/local/bin", filepath.Join(root, "..", "elsewhere"), root + "-sibling"} {
		if _, err := parse([]string{"--reclaim", outside}); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("%s: err = %v, want a refusal", outside, err)
		}
	}
	if _, err := parse([]string{"--uid", "2001", "--workspace", inside, "--home", "/tmp/elsewhere", "--", "true"}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a home outside the root was accepted: %v", err)
	}
}

// The launcher switches to pool users only, in their own group: a foreign
// group would hand the agent whatever that group may read.
func TestParseKeepsToThePoolUsers(t *testing.T) {
	for _, args := range [][]string{
		{"--uid", "3000", "--reclaim", "/w"},
		{"--uid", "1999", "--reclaim", "/w"},
		{"--gid", "1000", "--reclaim", "/w"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
	inv, err := parse([]string{"--uid", "2063", "--reclaim", "/w"})
	if err != nil || inv.uid != 2063 || inv.gid != 2000 {
		t.Fatalf("the last pool user was refused: %v (%+v)", err, inv)
	}
	// The probe's user checks, and never launches.
	if _, err := parse([]string{"--uid", "2000", "--workspace", "/w", "--home", "/h", "--", "true"}); err == nil || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("a launch as the probe's user was accepted: %v", err)
	}
	if _, err := parse([]string{"--uid", "2000", "--check", "/etc/passwd"}); err != nil {
		t.Fatalf("a check as the probe's user was refused: %v", err)
	}
	// The tree root reaches the launcher but not the agent.
	env := agentEnv([]string{"PATH=/bin", treeRootEnv + "=/data/runs"}, "/h", 2001)
	for _, entry := range env {
		if strings.HasPrefix(entry, treeRootEnv+"=") {
			t.Fatalf("the tree root reached the agent: %v", env)
		}
	}
}
