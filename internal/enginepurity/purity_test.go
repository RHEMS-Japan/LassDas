// Package enginepurity enforces the engine/instance split: the framework's
// working tree must never contain a consumer's identifiers. Consumer values
// (repository names, tracker project codes, numeric IDs, service hostnames)
// belong to instance repositories; when one leaks back into the engine it
// couples every future instance to the first customer and poisons an OSS
// release. This test is the CI gate that makes the promise mechanical.
package enginepurity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// forbiddenTokens are matched case-insensitively against every tracked file.
// The list itself is a consumer's identifiers, so it must not live in this
// tree either: a public engine carrying its own forbidden-word list would
// publish the very names it protects. Operators provide the list through the
// environment (a CI secret); where nobody provides one, the scan reports
// itself skipped instead of silently passing.
func forbiddenTokens(t *testing.T) []string {
	t.Helper()
	raw := os.Getenv("ENGINE_PURITY_TOKENS")
	if strings.TrimSpace(raw) == "" {
		t.Skip("ENGINE_PURITY_TOKENS is not set; the consumer-identifier scan runs only where the operator provides the list")
	}
	tokens := make([]string, 0, 8)
	for _, token := range strings.Split(raw, ",") {
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "" {
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		t.Fatal("ENGINE_PURITY_TOKENS is set but holds no tokens")
	}
	return tokens
}

func TestWorkingTreeContainsNoConsumerIdentifiers(t *testing.T) {
	root := repositoryRoot(t)
	listing, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	tokens := forbiddenTokens(t)
	for _, name := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
		if name == "" {
			continue
		}
		// A path is tree content too: a clean file under an identifying name
		// leaks just the same. Every printed path is redacted against the
		// list first - this log is public where the engine is, and printing
		// the identifier would republish the very value this gate keeps out.
		printable := redactTokens(name, tokens)
		for position, token := range tokens {
			if strings.Contains(strings.ToLower(name), token) {
				t.Errorf("%s is a tracked path containing consumer identifier #%d — rename it or move it to the instance repository", printable, position+1)
			}
		}
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", printable, err)
		}
		content := strings.ToLower(string(raw))
		for position, token := range tokens {
			if index := strings.Index(content, token); index >= 0 {
				line := 1 + strings.Count(content[:index], "\n")
				t.Errorf("%s:%d contains consumer identifier #%d — move the value to the instance repository", printable, line, position+1)
			}
		}
	}
}

// redactTokens blanks every occurrence of a forbidden token inside a printed
// path with its position marker, case-insensitively.
func redactTokens(name string, tokens []string) string {
	lowered := strings.ToLower(name)
	for position, token := range tokens {
		marker := "[#" + strconv.Itoa(position+1) + "]"
		for {
			index := strings.Index(lowered, token)
			if index < 0 {
				break
			}
			name = name[:index] + marker + name[index+len(token):]
			lowered = lowered[:index] + marker + lowered[index+len(token):]
		}
	}
	return name
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	output, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("git rev-parse --show-toplevel: %v", err)
	}
	return strings.TrimSpace(string(output))
}
