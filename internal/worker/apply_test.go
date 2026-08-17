package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func applyFixture(t *testing.T) (string, Config, TicketRequest, SourceSnapshot, Candidate) {
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
	if err := os.WriteFile(filename, []byte("export const label = 'Old label';\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	source, err := ReadSourceSnapshot(root, strings.Repeat("a", 40), request, config)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := NewCandidate(1, ModelCandidateOutput{
		Files: []ModelCandidateFile{{
			Path: request.TargetFiles[0], Content: "export const label = 'Updated label';\n",
		}},
		Rationale: "Update the requested visible label.",
	}, source, request, config, validTestInvocation(config.Models.Implementer), testInvocationTime)
	if err != nil {
		t.Fatal(err)
	}
	return root, config, request, source, candidate
}

func TestApplyCandidateAndVerifyExactContent(t *testing.T) {
	root, config, request, source, candidate := applyFixture(t)
	if err := ApplyCandidate(root, candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApplied(root, candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(filename))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(filename) {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func TestApplyCandidateRejectsStaleSourceWithoutMutation(t *testing.T) {
	root, config, request, source, candidate := applyFixture(t)
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	stale := []byte("export const label = 'Concurrent edit';\n")
	if err := os.WriteFile(filename, stale, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCandidate(root, candidate, source, request, config); err == nil {
		t.Fatal("ApplyCandidate() accepted a stale source")
	}
	content, err := os.ReadFile(filename)
	if err != nil || string(content) != string(stale) {
		t.Fatalf("content = %q, error = %v", content, err)
	}
}

func TestApplyCandidateRejectsSymlinkWithoutWritingOutside(t *testing.T) {
	root, config, request, source, candidate := applyFixture(t)
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	outside := filepath.Join(t.TempDir(), "outside.tsx")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filename); err != nil {
		t.Fatal(err)
	}
	if err := ApplyCandidate(root, candidate, source, request, config); err == nil {
		t.Fatal("ApplyCandidate() accepted a symlink")
	}
	content, err := os.ReadFile(outside)
	if err != nil || string(content) != "outside" {
		t.Fatalf("outside content = %q, error = %v", content, err)
	}
}

func TestApplyCandidateRejectsTamperedArtifacts(t *testing.T) {
	root, config, request, source, candidate := applyFixture(t)
	tampered := candidate
	tampered.Files = append([]CandidateFile(nil), candidate.Files...)
	tampered.Files[0].Content = "model-controlled tampering\n"
	if err := ApplyCandidate(root, tampered, source, request, config); err == nil {
		t.Fatal("ApplyCandidate() accepted a tampered candidate")
	}
}

func TestVerifyAppliedRequiresCompleteByteMatch(t *testing.T) {
	root, config, request, source, candidate := applyFixture(t)
	if err := ApplyCandidate(root, candidate, source, request, config); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, filepath.FromSlash(request.TargetFiles[0]))
	if err := os.WriteFile(filename, append([]byte(candidate.Files[0].Content), ' '), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := VerifyApplied(root, candidate, source, request, config); err == nil {
		t.Fatal("VerifyApplied() accepted non-exact content")
	}
}
