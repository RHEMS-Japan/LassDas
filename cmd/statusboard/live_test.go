package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveFixture adds live files to the ticket fixture's run directory.
func liveFixture(t *testing.T, files map[string]string) *boardServer {
	t.Helper()
	server := ticketFixture(t)
	dir := filepath.Join(filepath.Dir(filepath.Clean(server.statusDir)), "runs", "delivery_abc", "live")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &server
}

func liveGet(t *testing.T, server *boardServer, path string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	server.serveTicketAPI(rec, httptest.NewRequest("GET", path, nil))
	var payload map[string]any
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: body is not JSON: %s", path, rec.Body)
		}
	}
	return rec.Code, payload
}

func TestLiveIndexNamesTheStageOfEachStep(t *testing.T) {
	server := liveFixture(t, map[string]string{
		"read-contract.log": "受付の読み取りです\n",
		"agent-review.log":  "審査の出力です\n",
		"notes.txt":         "not a log",
	})
	code, payload := liveGet(t, server, "/api/tickets/PROJ-7/live")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	steps, _ := payload["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps = %v, want the two log files only", payload["steps"])
	}
	stages := map[string]string{}
	for _, entry := range steps {
		row := entry.(map[string]any)
		stages[row["step"].(string)], _ = row["stage"].(string)
	}
	if stages["read-contract"] != "intake" || stages["agent-review"] != "review" {
		t.Fatalf("stages = %v", stages)
	}
}

// A reader follows a step by carrying the offset it last read. Only whole
// lines are served, so a value can never be cut in half by the window.
func TestLiveServesFromTheOffsetAndKeepsWholeLines(t *testing.T) {
	server := liveFixture(t, map[string]string{"agent-review.log": "一行目\n二行目\n書きかけ"})
	code, first := liveGet(t, server, "/api/tickets/PROJ-7/live/agent-review")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if first["text"] != "一行目\n二行目\n" {
		t.Fatalf("text = %q, want whole lines only", first["text"])
	}
	next := int64(first["next"].(float64))
	code, second := liveGet(t, server, "/api/tickets/PROJ-7/live/agent-review?from="+itoa(next))
	if code != 200 || second["text"] != "" {
		t.Fatalf("a follow-up read returned %v (status %d), want nothing new", second["text"], code)
	}
	if int64(second["from"].(float64)) != next || int64(second["next"].(float64)) != next {
		t.Fatalf("from/next = %v/%v, want the unfinished line left where it is", second["from"], second["next"])
	}
}

// An offset the caller chose inside a line is moved to the next line, so a
// window can never open in the middle of a masked value: asking from=12 of
// "export KEY=sk-…" returned the rest of the key (review of #187).
func TestLiveRefusesToServeFromInsideALine(t *testing.T) {
	secret := "sk-" + strings.Repeat("C", 40)
	server := liveFixture(t, map[string]string{"agent-review.log": "export KEY=" + secret + "\n次の行\n"})
	for _, from := range []int64{1, 12, 20, 40} {
		code, payload := liveGet(t, server, "/api/tickets/PROJ-7/live/agent-review?from="+itoa(from))
		if code != 200 {
			t.Fatalf("from=%d: status %d", from, code)
		}
		text := payload["text"].(string)
		if strings.Contains(text, "C"+strings.Repeat("C", 20)) || strings.Contains(text, "sk-") {
			t.Fatalf("from=%d served part of the key: %q", from, text)
		}
		if text != "次の行\n" {
			t.Fatalf("from=%d served %q, want the next whole line", from, text)
		}
	}
}

func TestLiveMasksSecretsAndRefusesUnknownSteps(t *testing.T) {
	server := liveFixture(t, map[string]string{"agent-review.log": "export TOKEN=sk-live-abcdefghijklmnopqrstuvwxyz0123456789\n"})
	code, payload := liveGet(t, server, "/api/tickets/PROJ-7/live/agent-review")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if strings.Contains(payload["text"].(string), "sk-live-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("a secret was served: %q", payload["text"])
	}
	for _, path := range []string{"/api/tickets/PROJ-7/live/../board", "/api/tickets/PROJ-7/live/nope", "/api/tickets/PROJ-7/live/a%2Fb"} {
		rec := httptest.NewRecorder()
		server.serveTicketAPI(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code == 200 {
			t.Fatalf("%s was served: %s", path, rec.Body)
		}
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// The run directory is the agent's own workspace, so a link planted in it
// must not be followed out of the run (review of #187).
func TestLiveRefusesALinkInTheLiveDirectory(t *testing.T) {
	server := liveFixture(t, map[string]string{"real.log": "本物です\n"})
	dir := filepath.Join(filepath.Dir(filepath.Clean(server.statusDir)), "runs", "delivery_abc", "live")
	outside := filepath.Join(t.TempDir(), "host-file")
	if err := os.WriteFile(outside, []byte("HOST FILE CONTENTS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "planted.log")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rec := httptest.NewRecorder()
	server.serveTicketAPI(rec, httptest.NewRequest("GET", "/api/tickets/PROJ-7/live/planted", nil))
	if rec.Code == 200 {
		t.Fatalf("a link was served: %s", rec.Body)
	}
	_, index := liveGet(t, server, "/api/tickets/PROJ-7/live")
	for _, entry := range index["steps"].([]any) {
		if entry.(map[string]any)["step"] == "planted" {
			t.Fatal("a link was listed as a step")
		}
	}
}
