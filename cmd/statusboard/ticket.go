package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/ticketview"
)

// The ticket page: one ticket, everything its run recorded, behind the
// board's own credentials. /tickets/<key> serves the page, /api/tickets/<key>
// its data (the board row for "where it is now" plus ticketview's timeline),
// and /api/tickets/<key>/records/<name> one raw record, masked, for the
// operator who wants the original. The key is resolved to a run directory
// through board.json - the attendant's own list of runs - never through the
// path, so the run directories are read only by names the board knows.

//go:embed ticket.html
var ticketPage []byte

var ticketKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}-[0-9]{1,8}$`)
var recordNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,48}$`)

// boardRow is the board.json row as the ticket page needs it.
type boardRow struct {
	DeliveryID   string          `json:"delivery_id"`
	IssueID      int64           `json:"issue_id"`
	IssueKey     string          `json:"issue_key"`
	Summary      string          `json:"summary"`
	Step         string          `json:"step"`
	StepTitle    string          `json:"step_title"`
	Detail       string          `json:"detail"`
	NextAction   string          `json:"next_action"`
	ActionEffect string          `json:"action_effect"`
	Terminal     string          `json:"terminal_code"`
	CanGo        bool            `json:"can_go"`
	CanResolve   bool            `json:"can_resolve"`
	PRURL        string          `json:"pr_url"`
	Raw          json.RawMessage `json:"-"`
}

// runsRoot is where the run directories live: LASSDAS_RUNS_ROOT, or the
// "runs" directory beside the status directory (the pod's /data layout).
func (s *boardServer) runsRoot() string {
	if root := os.Getenv("LASSDAS_RUNS_ROOT"); root != "" {
		return root
	}
	return filepath.Join(filepath.Dir(filepath.Clean(s.statusDir)), "runs")
}

// boardRowFor finds the board row of an issue key. Only rows the attendant
// wrote are served; an unknown key is a 404, not a directory probe.
func (s *boardServer) boardRowFor(key string) (boardRow, bool) {
	raw, err := os.ReadFile(filepath.Join(s.statusDir, "board.json"))
	if err != nil {
		return boardRow{}, false
	}
	var board struct {
		Runs []json.RawMessage `json:"runs"`
	}
	if json.Unmarshal(raw, &board) != nil {
		return boardRow{}, false
	}
	var found boardRow
	var haveRow bool
	for _, entry := range board.Runs {
		var row boardRow
		if json.Unmarshal(entry, &row) != nil || row.IssueKey != key || row.DeliveryID == "" {
			continue
		}
		row.Raw = entry
		// Several runs may carry one key (a re-run); the newest row wins,
		// and the attendant lists rows newest last.
		found, haveRow = row, true
	}
	return found, haveRow
}

func ticketKeyFromPath(path, prefix string) (key, rest string, ok bool) {
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	tail := strings.TrimPrefix(path, prefix)
	key, rest, _ = strings.Cut(tail, "/")
	if !ticketKeyPattern.MatchString(key) {
		return "", "", false
	}
	return key, rest, true
}

func (s *boardServer) serveTicketPage(w http.ResponseWriter, r *http.Request) {
	if _, rest, ok := ticketKeyFromPath(r.URL.Path, "/tickets/"); !ok || rest != "" {
		http.NotFound(w, r)
		return
	}
	writePage(w, ticketPage)
}

func (s *boardServer) serveTicketAPI(w http.ResponseWriter, r *http.Request) {
	key, rest, ok := ticketKeyFromPath(r.URL.Path, "/api/tickets/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	row, found := s.boardRowFor(key)
	if !found {
		http.Error(w, "この票は盤面にありません", http.StatusNotFound)
		return
	}
	runDir := filepath.Join(s.runsRoot(), filepath.Base(row.DeliveryID))
	if rest != "" {
		s.serveTicketRecord(w, r, runDir, rest)
		return
	}
	view, err := ticketview.Build(runDir)
	if err != nil {
		// The board row alone is still worth showing: the run directory
		// may be gone (retired) while the row remains.
		view = ticketview.View{DeliveryID: row.DeliveryID, Timeline: []ticketview.Event{}, Records: []string{}}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(struct {
		Board       json.RawMessage `json:"board"`
		View        ticketview.View `json:"view"`
		TrackerBase string          `json:"tracker_base,omitempty"`
		Actions     bool            `json:"actions_enabled"`
	}{Board: row.Raw, View: view, TrackerBase: s.trackerBase, Actions: s.poster != nil})
}

// serveTicketRecord serves one raw record by its page name, masked. The
// name must be one ticketview serves; the file is read from the run
// directory the board row named, never from a path the client wrote.
func (s *boardServer) serveTicketRecord(w http.ResponseWriter, r *http.Request, runDir, rest string) {
	const prefix = "records/"
	if !strings.HasPrefix(rest, prefix) {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(rest, prefix)
	if !recordNamePattern.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	relative := ticketview.RecordPath(name)
	if relative == "" {
		http.NotFound(w, r)
		return
	}
	raw, err := os.ReadFile(filepath.Join(runDir, relative))
	if err != nil {
		http.Error(w, "この記録は残っていません", http.StatusNotFound)
		return
	}
	const maxRecord = 512 << 10
	if len(raw) > maxRecord {
		raw = raw[:maxRecord]
	}
	masked, _, refusal := probe.MaskSecrets(string(raw), nil)
	if refusal != "" {
		http.Error(w, "この記録は秘密の形 ("+refusal+") を含むため表示しません", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(masked))
}
