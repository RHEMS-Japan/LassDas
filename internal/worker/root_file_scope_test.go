package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRootFileScopeListsExactRegularFiles(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = []string{"README.md", "main.go", "server/", "missing.go"}
	root := deriveTestTree(t)
	if err := os.Symlink("server/main.go", filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	listing, err := ReadCandidateListing(root, strings.Repeat("a", 40), config.Consumers[0], config)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"README.md", "server/main.go"}; !reflect.DeepEqual(listing.Paths, want) {
		t.Fatalf("got %v, want %v", listing.Paths, want)
	}
	for _, entry := range []string{"", ".", "..", "/main.go", "../main.go", ".env", ".github", "main*", "cmd/main.go"} {
		config.Consumers[0].Mode.AllowedFilePrefixes = []string{entry}
		if err := config.Validate(); err == nil {
			t.Errorf("accepted invalid root entry %q", entry)
		}
	}
}

func TestRootFileScopeSurvivesTicketCandidateAndAgentChecks(t *testing.T) {
	config := validTestConfig()
	config.Consumers[0].Mode.AllowedFilePrefixes = []string{"main.go", "client/src/"}
	description := strings.ReplaceAll(validTicketDescription(), "client/src/components/Example.tsx", "main.go")
	request, err := ParseTicket(validTicketEnvelope(t, description), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("Old label\n"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{Files: []ModelCandidateFile{{Path: "main.go", Content: "Updated label\n"}}, Rationale: "Update the requested label."}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Validate(source, request, config); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "init")
	runTestGit(t, root, "add", "main.go")
	runTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("Updated label\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := ChangedFilesUnder(root, config.Consumers[0].Mode.AllowedFilePrefixes, nil)
	if err != nil || !reflect.DeepEqual(changed, []string{"main.go"}) {
		t.Fatalf("changed = %v, error = %v", changed, err)
	}
	if _, err := ReadObservedChanges(root, t.TempDir(), changed, config.Consumers[0]); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main.go.bak", "main.go/child", "other/main.go", ".github/workflows/ci.yml"} {
		description := strings.ReplaceAll(validTicketDescription(), "client/src/components/Example.tsx", name)
		if _, err := ParseTicket(validTicketEnvelope(t, description), config); err == nil {
			t.Errorf("ticket accepted %q", name)
		}
		if allowedPath(name, config.Consumers[0].Mode.AllowedFilePrefixes) {
			t.Errorf("scope accepted %q", name)
		}
		if _, err := ReadObservedChanges(root, t.TempDir(), []string{name}, config.Consumers[0]); err == nil {
			t.Errorf("agent accepted %q", name)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "main.go.bak"), []byte("extra"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ChangedFilesUnder(root, config.Consumers[0].Mode.AllowedFilePrefixes, nil); err == nil {
		t.Fatal("agent change checker accepted neighboring root file")
	}
	if err := os.Remove(filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go.bak", filepath.Join(root, "main.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadObservedChanges(root, t.TempDir(), []string{"main.go"}, config.Consumers[0]); err == nil {
		t.Fatal("agent change reader accepted root symlink")
	}
}
