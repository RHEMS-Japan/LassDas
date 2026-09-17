package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/livelog"
)

// What the agent says arrives in the live file while it is still running,
// not only in the transcript the record keeps when it ends. A reader
// watching a stage that produces nothing for ninety seconds cannot tell it
// from a stage that died (live 2026-09-17).
func TestAgentOutputReachesTheLiveFile(t *testing.T) {
	root, _ := buildAgentRepository(t)
	path := filepath.Join(t.TempDir(), "live", "implement.log")
	t.Setenv(livelog.PathEnv, path)
	name, _ := writeFakeAgent(t, "echo '実装を始めます'; printf \"export const submitLabel = 'Submit';\\n\" > client/src/label.ts; echo '変更しました'")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")

	outcome, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", []string{"client/src/"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the live file was not written: %v", err)
	}
	body := string(raw)
	for _, line := range []string{"実装を始めます", "変更しました"} {
		if !strings.Contains(body, line) {
			t.Fatalf("live file %q lacks %q", body, line)
		}
	}
	if !strings.Contains(outcome.Transcript, "変更しました") {
		t.Fatalf("the transcript lost what the live view showed: %q", outcome.Transcript)
	}
}

// A secret the agent prints never reaches the live file, which is served
// over HTTP to whoever can open the board.
func TestAgentSecretsDoNotReachTheLiveFile(t *testing.T) {
	root, _ := buildAgentRepository(t)
	path := filepath.Join(t.TempDir(), "live", "implement.log")
	t.Setenv(livelog.PathEnv, path)
	name, _ := writeFakeAgent(t, "echo \"TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789\"")
	t.Setenv("FIXTURE_AGENT_CREDENTIAL", "secret-value")

	if _, err := RunAgent(context.Background(), fixtureAgentConfig("author-agent", name), root, "do the thing", []string{"client/src/"}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the live file was not written: %v", err)
	}
	if strings.Contains(string(raw), "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("a token reached the live file: %q", raw)
	}
}
