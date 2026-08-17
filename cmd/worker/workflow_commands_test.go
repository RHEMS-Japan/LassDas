package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The workflow calls this binary through shell, so a renamed subcommand or
// flag is invisible to the compiler and surfaces only as a live run that dies
// with "arguments are invalid". These tests read the workflow and the command
// definitions and fail when the two drift apart.
const workflowPath = "../../.github/workflows/m1-worker.yml"

func TestWorkflowCallsCommandsAndFlagsThisBinaryDefines(t *testing.T) {
	workflow := readWorkflowText(t, workflowPath)
	handlers := commandHandlers(t)
	flagsByHandler := flagsPerHandler(t)

	invocations := workflowWorkerInvocations(workflow)
	if len(invocations) == 0 {
		t.Fatal("no worker invocations were found in the workflow")
	}
	for command, used := range invocations {
		handler, known := handlers[command]
		if !known {
			t.Fatalf("the workflow runs %q, which this binary does not accept", command)
		}
		declared, found := flagsByHandler[handler]
		if !found {
			t.Fatalf("%s handles %q but declares no flags", handler, command)
		}
		for flag := range used {
			if _, accepted := declared[flag]; !accepted {
				t.Fatalf("the workflow passes --%s to %q, which does not accept it", flag, command)
			}
		}
	}
}

func readWorkflowText(t *testing.T, path string) string {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// workflowWorkerInvocations collects each worker subcommand the workflow runs
// together with the flags it passes. A call continues across lines while they
// end in a backslash, which is how the workflow writes them.
func workflowWorkerInvocations(workflow string) map[string]map[string]struct{} {
	invocations := make(map[string]map[string]struct{})
	call := regexp.MustCompile(`"\$(?:TOOLS|sandbox/tools)/worker" ([a-z-]+)`)
	flag := regexp.MustCompile(`(?:^|\s)--([a-z-]+)`)
	lines := strings.Split(workflow, "\n")
	for index, line := range lines {
		match := call.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		command := match[1]
		if invocations[command] == nil {
			invocations[command] = map[string]struct{}{}
		}
		for cursor := index; cursor < len(lines) && strings.HasSuffix(strings.TrimRight(lines[cursor], " "), `\`); cursor++ {
			for _, found := range flag.FindAllStringSubmatch(lines[cursor+1], -1) {
				invocations[command][found[1]] = struct{}{}
			}
		}
	}
	return invocations
}

// commandHandlers reads the dispatch table: which function runs each
// subcommand the binary accepts.
func commandHandlers(t *testing.T) map[string]string {
	t.Helper()
	source := readWorkflowText(t, filepath.Join("main.go"))
	dispatch := regexp.MustCompile(`case "([a-z-]+)":\n\s+return (run[A-Za-z]+)\(`)
	handlers := make(map[string]string)
	for _, match := range dispatch.FindAllStringSubmatch(source, -1) {
		handlers[match[1]] = match[2]
	}
	if len(handlers) == 0 {
		t.Fatal("no subcommands were found in the dispatch table")
	}
	return handlers
}

// flagsPerHandler reads the flags each handler declares. Declarations sit
// inside the function that runs the command, so the body is scanned from its
// signature to the next one.
func flagsPerHandler(t *testing.T) map[string]map[string]struct{} {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	declaration := regexp.MustCompile(`flags\.(?:String|Int|Bool|Int64)\("([a-z-]+)"|flags\.Var\(&[A-Za-z]+, "([a-z-]+)"`)
	signature := regexp.MustCompile(`^func (run[A-Za-z]+)\(`)
	declared := make(map[string]map[string]struct{})
	for _, entry := range entries {
		if strings.HasSuffix(entry, "_test.go") {
			continue
		}
		handler := ""
		for _, line := range strings.Split(readWorkflowText(t, entry), "\n") {
			if match := signature.FindStringSubmatch(line); match != nil {
				handler = match[1]
				if declared[handler] == nil {
					declared[handler] = map[string]struct{}{}
				}
				continue
			}
			if handler == "" {
				continue
			}
			for _, match := range declaration.FindAllStringSubmatch(line, -1) {
				name := match[1]
				if name == "" {
					name = match[2]
				}
				declared[handler][name] = struct{}{}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("no flag declarations were found")
	}
	return declared
}
