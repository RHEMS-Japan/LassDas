package worker

import (
	"os"
	"path/filepath"
	"testing"
)

func locateTestTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// The requester writes what the wording is now and what it should become.
// Which file holds it is not something they should have to know.
func TestLocateTargetFilesFindsTheWordingWithoutAModel(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t) // Absent-Text: "Old label"
	root := locateTestTree(t, map[string]string{
		"client/src/components/Settings.tsx": "export const heading = 'Old label';\n",
		"client/src/components/Other.tsx":    "export const heading = 'Something else';\n",
		"server/main.go":                     "const heading = \"Old label\"\n",
		"README.md":                          "Old label\n",
	})
	location, err := LocateTargetFiles(root, draft, config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 1 || location.Matches[0] != "client/src/components/Settings.tsx" {
		t.Fatalf("matches = %v, want only the one inside the writable scope", location.Matches)
	}
	request, err := location.Resolve(draft, config)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if err := request.Validate(config); err != nil {
		t.Fatalf("resolved contract is invalid: %v", err)
	}
	if len(request.TargetFiles) != 1 || request.TargetFiles[0] != "client/src/components/Settings.tsx" {
		t.Fatalf("target files = %v", request.TargetFiles)
	}
}

func TestLocateTargetFilesRefusesToGuess(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)

	// Nothing to replace: the wording is not where this automation may write,
	// so the run must stop rather than change something adjacent.
	absent := locateTestTree(t, map[string]string{"client/src/index.tsx": "export const heading = 'Unrelated';\n"})
	location, err := LocateTargetFiles(absent, draft, config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 0 {
		t.Fatalf("matches = %v, want none", location.Matches)
	}
	if _, err := location.Resolve(draft, config); err == nil {
		t.Fatal("a contract was resolved with nothing to change")
	}

	// More occurrences than the mode may change in one run: the requester has
	// to say which they meant, so the automation must not pick for them.
	many := map[string]string{}
	for _, name := range []string{"a", "b", "c", "d"} {
		many["client/src/"+name+".tsx"] = "export const heading = 'Old label';\n"
	}
	crowded := locateTestTree(t, many)
	location, err = LocateTargetFiles(crowded, draft, config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 4 {
		t.Fatalf("matches = %v, want all four reported", location.Matches)
	}
	if _, err := location.Resolve(draft, config); err == nil {
		t.Fatal("a contract was resolved beyond the file budget")
	}
}

func TestLocateTargetFilesNeverLeavesTheWritableScope(t *testing.T) {
	config := validTestConfig()
	draft := deriveTestDraft(t)
	root := locateTestTree(t, map[string]string{
		"client/src/ok.tsx": "export const heading = 'Old label';\n",
		"server/secret.go":  "const heading = \"Old label\"\n",
	})
	// A symlink from inside the scope to a file outside it must not turn the
	// outside file into a target.
	link := filepath.Join(root, "client", "src", "linked.tsx")
	if err := os.Symlink(filepath.Join(root, "server", "secret.go"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	location, err := LocateTargetFiles(root, draft, config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 1 || location.Matches[0] != "client/src/ok.tsx" {
		t.Fatalf("matches = %v, want only the real file inside the scope", location.Matches)
	}
}
