package chain

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Real process + public adapter; the stand-in only implements the SDK boundary.
// Real native execution/model judgment is a separate live experiment.
func TestNativeBridgeProcessKeepsPartialReportAndFailureObservation(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("native Python bridge needs python3")
	}
	bridge, err := filepath.Abs("../../harnesses/hermes.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const native = `import os
print("native import diagnostic")
assert os.environ["PYTHON_DOTENV_DISABLED"] == "1"
class AIAgent:
 def __init__(self, **config):
  assert config["model"] == "example/current-model"
  print("native startup diagnostic")
 def run_conversation(self, user_message):
  print("tool diagnostic")
  return {"final_response": "Partial work: " + user_message, "failed": True,
          "error": "provider unavailable " + os.environ["OPENROUTER_API_KEY"]}
 def close(self):
  print("native cleanup diagnostic")
`
	if err := os.WriteFile(filepath.Join(dir, "run_agent.py"), []byte(native), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_NATIVE_CREDENTIAL", "synthetic-native-value")
	process := Process{Name: "worker", Directory: dir, Command: []string{python, bridge},
		Env: map[string]string{
			"PYTHONPATH": dir, "PYTHONDONTWRITEBYTECODE": "1", "HERMES_HOME": filepath.Join(dir, "role"),
			"OPENROUTER_BASE_URL": "https://model.example/api/v1", "NATIVE_MODEL": "example/current-model",
		}, Secrets: map[string]string{"OPENROUTER_API_KEY": "TEST_NATIVE_CREDENTIAL"},
	}
	const original = "元の依頼をそのまま。\nUnknown: {\"extra\":true}\nLiteral $(exit 9)."
	role, assignment := Role{Name: "implement"}, Assignment{Role: "implement", Instruction: "Inspect the actual work."}
	state := State{Request: original}
	result := process.run(context.Background(), role, assignment, state)
	if want := "Partial work: " + processPrompt(role, process, assignment, state); result.Output != want {
		t.Fatalf("report or original request changed: %q", result.Output)
	}
	if !strings.Contains(result.Error, "exit status 1") || !strings.Contains(result.Error, "provider unavailable [credential]") {
		t.Fatalf("native failure reason missing: %q", result.Error)
	}
	for _, part := range []string{"import", "startup", "tool", "cleanup"} {
		if !strings.Contains(result.Diagnostics, part+" diagnostic") {
			t.Errorf("missing %s diagnostic", part)
		}
	}
	if strings.Contains(result.Output+result.Diagnostics+result.Error, "synthetic-native-value") {
		t.Fatal("native credential leaked into history")
	}
}
