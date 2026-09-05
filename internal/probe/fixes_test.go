package probe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecorderNormalisesInvalidUTF8BeforeHashing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	// A multibyte character cut in half, as an output cap can leave it.
	cut := strings.Repeat("日本語", 3)
	cut = cut[:len(cut)-1]
	now := time.Now().UTC()
	if _, err := recorder.Append(Measurement{Probe: "repo.read", StartedAt: now, EndedAt: now, Output: cut}); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyPrefix(path, 1); err != nil {
		t.Fatalf("line with a cut character does not re-derive: %v", err)
	}
	if _, err := OpenRecorder(path); err != nil {
		t.Fatalf("reopen after a cut character: %v", err)
	}
	stored, err := ReadPrefix(path, 1)
	if err != nil || !strings.HasSuffix(stored[0].Output, "�") || stored[0].OutputSHA256 != digestHex([]byte(stored[0].Output)) {
		t.Errorf("stored output not normalised: %+v %v", stored, err)
	}
}

func TestExecProbeNeverInheritsTheKernelEnvironment(t *testing.T) {
	t.Setenv("PROBE_TEST_SECRET", "must-not-leak")
	catalog, err := NewCatalog([]Spec{{ID: "env", Kind: KindExec, Argv: []string{"env"}}})
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder}
	outcome, err := session.Run(context.Background(), Request{Probe: "env"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outcome.Excerpt, "PROBE_TEST_SECRET") || strings.Contains(outcome.Excerpt, "must-not-leak") {
		t.Errorf("child inherited the kernel environment: %q", outcome.Excerpt)
	}
}

func TestRepoProbesNeverReadTheGitDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[http]\n\textraheader = Authorization: basic dXNlcjpwYXNz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, _ := NewCatalog(nil)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root}
	for _, request := range []Request{
		{Probe: "repo.read", Args: map[string]string{"path": ".git/config"}},
		{Probe: "repo.list", Args: map[string]string{"path": ".git"}},
		{Probe: "repo.grep", Args: map[string]string{"pattern": "extraheader", "path": ".git"}},
	} {
		outcome, err := session.Run(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(outcome.Excerpt, "extraheader") || outcome.Measurement.ExitCode == 0 {
			t.Errorf("%s read the git directory: %+v %q", request.Probe, outcome.Measurement, outcome.Excerpt)
		}
	}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.grep", Args: map[string]string{"pattern": "extraheader"}})
	if err != nil || strings.Contains(outcome.Excerpt, "extraheader") {
		t.Errorf("grep from the root walked into .git: %q %v", outcome.Excerpt, err)
	}
	if outcome, _ := session.Run(context.Background(), Request{Probe: "repo.list"}); !strings.Contains(outcome.Excerpt, "main.go") || strings.Contains(outcome.Excerpt, ".git/") {
		t.Errorf("list: %q", outcome.Excerpt)
	}
}

func TestHTTPProbePathMustBeAPath(t *testing.T) {
	catalog, err := NewCatalog([]Spec{{ID: "http.timing", Kind: KindHTTP, Hosts: []string{"app-stg.example.invalid"}, Methods: []string{"GET"},
		Args: map[string]string{"path": `[A-Za-z0-9:/@_.-]{0,40}`}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{":8443/x", "//other.example.invalid/x", "/x@y", "x", "/a\\b"} {
		if _, refusal := catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"path": path}}); refusal == nil {
			t.Errorf("path %q accepted even though the consumer's pattern allowed it", path)
		}
	}
	if _, refusal := catalog.Resolve(Request{Probe: "http.timing", Args: map[string]string{"path": "/console/new"}}); refusal != nil {
		t.Errorf("plain path refused: %v", refusal)
	}
}

func TestSessionFillsPartialLimits(t *testing.T) {
	catalog, _ := NewCatalog(nil)
	recorder, err := OpenRecorder(filepath.Join(t.TempDir(), "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	session := &Session{Catalog: catalog, Recorder: recorder, RepoRoot: root, Limits: Limits{MaxProbes: 5}}
	outcome, err := session.Run(context.Background(), Request{Probe: "repo.read", Args: map[string]string{"path": "a.txt"}})
	if err != nil || outcome.Measurement.Refused || outcome.Excerpt != "hello" {
		t.Errorf("partial limits refused a plain read: %+v %q %v", outcome.Measurement, outcome.Excerpt, err)
	}
}
