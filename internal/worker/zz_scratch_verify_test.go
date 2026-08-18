package worker

import (
	"strings"
	"testing"
)

func scratchCommit(t *testing.T, root string) {
	t.Helper()
	agentGit(t, root, "add", ".gitignore")
	agentGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "ignore rule")
}

// (a) file-pattern ignore inside the writable scope must fail, naming the path.
func TestScratchA_PatternIgnoredCreatedFileIsNamed(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "*.generated.ts\n")
	scratchCommit(t, root)
	writeAgentFile(t, root, "client/src/messages.generated.ts", "export const x = 1;\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"})
	t.Logf("changed=%v err=%v", changed, err)
	if err == nil {
		t.Fatal("FAIL: ignored created file was silently dropped")
	}
	if !strings.Contains(err.Error(), "agent produced a file the repository ignores: client/src/messages.generated.ts") {
		t.Fatalf("FAIL: error does not name the path: %v", err)
	}
}

// (b) byproducts outside scope, and hidden ones inside it, stay tolerated.
func TestScratchB_ByproductsTolerated(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "node_modules/\n.DS_Store\n")
	scratchCommit(t, root)
	writeAgentFile(t, root, "node_modules/left/pad.js", "module.exports = 1\n")
	writeAgentFile(t, root, "client/src/.DS_Store", "junk\n")
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"})
	t.Logf("changed=%v err=%v", changed, err)
	if err != nil {
		t.Fatalf("FAIL: byproducts were treated as deliverables: %v", err)
	}
	if len(changed) != 1 || changed[0] != "client/src/label.ts" {
		t.Fatalf("FAIL: changed=%v", changed)
	}
}

// (c) a whole ignored DIRECTORY inside the scope: git reports "!! client/src/gen/".
func TestScratchC_DirectoryIgnoredCreatedFileIsCaught(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "client/src/gen/\n")
	scratchCommit(t, root)
	writeAgentFile(t, root, "client/src/gen/api.ts", "export const api = 1;\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"})
	t.Logf("changed=%v err=%v", changed, err)
	if err == nil {
		t.Fatal("FAIL: ignored directory inside scope was silently dropped")
	}
	if !strings.Contains(err.Error(), "ignores") {
		t.Fatalf("FAIL: unexpected error: %v", err)
	}
	t.Logf("NOTE: error names %q (directory, not the file inside)", err.Error())
}

// (c2) the ignored directory CONTAINS the writable scope (.gitignore has "client/").
func TestScratchC2_IgnoredDirectoryContainingScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "client/\n")
	scratchCommit(t, root)
	writeAgentFile(t, root, "client/src/added.ts", "export const a = 1;\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"})
	t.Logf("changed=%v err=%v", changed, err)
	if err == nil {
		t.Fatal("FAIL: ignored parent directory of the scope was silently dropped")
	}
}

// (d) no allowed prefixes at all (empty scope list) — is an ignored file still caught?
func TestScratchD_EmptyPrefixList(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "*.generated.ts\n")
	scratchCommit(t, root)
	writeAgentFile(t, root, "client/src/messages.generated.ts", "export const x = 1;\n")

	changed, err := ChangedFilesUnder(root, nil)
	t.Logf("changed=%v err=%v", changed, err)
	if err == nil {
		t.Fatal("FAIL: with no scope restriction the ignored file was dropped")
	}
}

// (e) ADVERSARIAL: an ignored artifact that already existed in the workspace
// before the agent ran (e.g. a build output dir the toolchain regenerates).
// The fix assumes a clean checkout; this measures what happens if that
// assumption does not hold.
func TestScratchE_PreexistingIgnoredArtifactInScope(t *testing.T) {
	root, _ := buildAgentRepository(t)
	writeAgentFile(t, root, ".gitignore", "client/src/dist/\n")
	scratchCommit(t, root)
	// Simulate a build the agent ran (or a stale artifact already present).
	writeAgentFile(t, root, "client/src/dist/bundle.js", "console.log(1)\n")
	// The agent's actual deliverable:
	writeAgentFile(t, root, "client/src/label.ts", "export const submitLabel = 'Submit';\n")

	changed, err := ChangedFilesUnder(root, []string{"client/src/"})
	t.Logf("changed=%v err=%v", changed, err)
	if err != nil {
		t.Logf("OBSERVED: a build artifact inside the scope now fails the run: %v", err)
	} else {
		t.Logf("OBSERVED: tolerated, changed=%v", changed)
	}
}
