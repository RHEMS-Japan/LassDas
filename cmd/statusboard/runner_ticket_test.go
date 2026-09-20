package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerDetailAndLiveResolveExistingWorkspaceWithoutExposingItsPath(t *testing.T) {
	s := ticketFixture(t)
	workspace := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(s.statusDir, "board.json"))
	if err != nil {
		t.Fatal(err)
	}
	var board map[string]any
	if err := json.Unmarshal(raw, &board); err != nil {
		t.Fatal(err)
	}
	row := board["runs"].([]any)[0].(map[string]any)
	row["workspace_path"] = workspace
	row["state"] = "claimed"
	row["step"] = "implement"
	raw, _ = json.Marshal(board)
	if err := os.WriteFile(filepath.Join(s.statusDir, "board.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	current, _ := json.Marshal(map[string]any{"step": "implement", "started_at": time.Now()})
	if err := os.WriteFile(filepath.Join(workspace, "current-step.json"), current, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, "live"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "live", "implement.log"), []byte("working now\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ path, want string }{{"/api/tickets/PROJ-7", `"step":"implement"`}, {"/api/tickets/PROJ-7/live/implement", "working now"}} {
		response := httptest.NewRecorder()
		s.serveTicketAPI(response, httptest.NewRequest("GET", test.path, nil))
		if response.Code != 200 || !strings.Contains(response.Body.String(), test.want) {
			t.Fatalf("%s: %d %s", test.path, response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), workspace) || strings.Contains(response.Body.String(), "workspace_path") {
			t.Fatal("private routing leaked")
		}
	}
}
