package worker

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scopeTestTree is an ordinary destination: a writable prefix with files in
// it, and files outside it that no step of the search may reach.
func scopeTestTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{
		"client/src/components/Settings.tsx",
		"client/src/components/Example.tsx",
		"client/src/index.tsx",
		"server/main.go",
		"README.md",
	} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("export const label = 'Old label';\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// wordingPromiseDraft is a ticket that promises a visible wording change, so
// its Absent-Text is what the search looks for.
func wordingPromiseDraft(t *testing.T) TicketDraft {
	t.Helper()
	draft, _, err := ParseTicketDraft(validTicketEnvelope(t, draftTicketDescription()), validTestConfig(), unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("ParseTicketDraft() error = %v", err)
	}
	return draft
}

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
	draft := wordingPromiseDraft(t) // Absent-Text: "Old label"
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
	draft := wordingPromiseDraft(t)

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
	draft := wordingPromiseDraft(t)
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

// The writable scope is the only place the search reads. Repository
// machinery and secrets sit under the same prefix and must be invisible to
// it: a dotted name is never the subject of a visible wording change, and
// reading one would put .git and .env inside the searchable scope.
func TestTheWordingSearchSkipsHiddenAndUnreadablePaths(t *testing.T) {
	config := validTestConfig()
	root := locateTestTree(t, map[string]string{
		"client/src/index.tsx":   "export const heading = 'Old label';\n",
		"client/src/.env":        "SECRET=Old label\n",
		"client/src/.git/config": "[core]\n\thooksPath = Old label\n",
	})
	location, err := LocateTargetFiles(root, wordingPromiseDraft(t), config)
	if err != nil {
		t.Fatalf("LocateTargetFiles() error = %v", err)
	}
	if len(location.Matches) != 1 || location.Matches[0] != "client/src/index.tsx" {
		t.Fatalf("matches = %v, want the search to skip dotted paths", location.Matches)
	}
	paths, err := writableScopePaths(root, config.Consumers[0])
	if err != nil {
		t.Fatalf("writableScopePaths() error = %v", err)
	}
	if !reflect.DeepEqual(paths, []string{"client/src/index.tsx"}) {
		t.Fatalf("paths = %v, want only the ordinary file", paths)
	}
}

// A root that is not there, or not a directory, is a broken run rather than
// an empty search: the difference decides whether a ticket is told its
// wording was not found.
func TestTheWordingSearchFailsClosedOnABrokenTree(t *testing.T) {
	config := validTestConfig()
	if _, err := writableScopePaths(filepath.Join(t.TempDir(), "missing"), config.Consumers[0]); err == nil {
		t.Fatal("a missing root produced a scope listing")
	}
	// A tree with nothing inside the writable prefix is a legitimate search
	// that finds nothing, not a failure.
	paths, err := writableScopePaths(t.TempDir(), config.Consumers[0])
	if err != nil || len(paths) != 0 {
		t.Fatalf("paths = %v, error = %v", paths, err)
	}
	// A symlink inside the scope is never searched, whatever it points at.
	root := scopeTestTree(t)
	if err := os.Symlink(filepath.Join(root, "server", "main.go"), filepath.Join(root, "client", "src", "linked.tsx")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	paths, err = writableScopePaths(root, config.Consumers[0])
	if err != nil {
		t.Fatalf("writableScopePaths() error = %v", err)
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "client/src/") {
			t.Fatalf("the scope listing left the writable prefix: %q", path)
		}
		if strings.HasSuffix(path, "linked.tsx") {
			t.Fatal("a symlink was listed inside the writable scope")
		}
	}
}
