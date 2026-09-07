package main

import (
	"bytes"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The launcher does exactly one thing per call, names the agent user it
// would switch to, and refuses to run anything as the caller itself.
func TestParseInsistsOnOneModeAndASeparateUser(t *testing.T) {
	inv, err := parse([]string{"--workspace", "/w", "--home", "/h", "--", "hermes", "--profile", "x"})
	if err != nil || inv.workspace != "/w" || inv.home != "/h" || strings.Join(inv.command, " ") != "hermes --profile x" || inv.uid != defaultAgentUID {
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
	env := agentEnv([]string{"HOME=/home/engine", "USER=engine", "PATH=/bin", "MODEL_KEY=k"}, "/data/agent-home")
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
