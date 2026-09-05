package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitSourceFixture(t *testing.T) (string, string, Config, TicketRequest) {
	t.Helper()
	config := validTestConfig()
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), config)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "init", "-q")
	runTestGit(t, root, "add", "--", request.TargetFiles[0])
	runTestGit(t, root, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
	baseSHA := strings.TrimSpace(runTestGit(t, root, "rev-parse", "HEAD"))
	return root, baseSHA, config, request
}

func runTestGit(t *testing.T, root string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root, "-c", "gc.auto=0", "-c", "maintenance.auto=false"}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func TestReadVerifiedSourceSnapshotBindsCleanGitTree(t *testing.T) {
	root, baseSHA, config, request := gitSourceFixture(t)
	snapshot, err := ReadVerifiedSourceSnapshot(context.Background(), root, baseSHA, request, config)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BaseSHA != baseSHA || snapshot.Files[0].GitBlobSHA == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestReadVerifiedSourceSnapshotRejectsDirtyOrWrongHEAD(t *testing.T) {
	root, baseSHA, config, request := gitSourceFixture(t)
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.WriteFile(filename, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerifiedSourceSnapshot(context.Background(), root, baseSHA, request, config); err == nil {
		t.Fatal("ReadVerifiedSourceSnapshot() accepted a dirty checkout")
	}
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerifiedSourceSnapshot(context.Background(), root, strings.Repeat("f", 40), request, config); err == nil {
		t.Fatal("ReadVerifiedSourceSnapshot() accepted a different base SHA")
	}
}
