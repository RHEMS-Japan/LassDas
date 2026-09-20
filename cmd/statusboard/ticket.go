package main

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/ticketview"
)

// The ticket page: one ticket, everything its run recorded, behind the
// board's own access gate. /tickets/<key> serves the page, /api/tickets/<key>
// its data (the board row for "where it is now" plus ticketview's timeline),
// and /api/tickets/<key>/records/<name> one raw record, masked, for the
// operator who wants the original. The key is resolved to a run directory
// through board.json - the attendant's own list of runs - never through the
// path, so the run directories are read only by names the board knows.

//go:embed ticket.html
var ticketPage []byte

// ticketKeyPattern is the engine's own issue key shape (worker config).
var ticketKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}-[1-9][0-9]*$`)

// liveStepPattern is the file-name shape the engine writes live output
// under: one path segment of the characters runner.LiveLogName produces.
var liveStepPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)

var recordNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,96}$`)

// maxRecordRead bounds what one record request reads. A record larger
// than that is refused whole, never cut: the secret scan must see a whole
// file, or nothing (a value split at a cut would lose the shape the scan
// looks for). The bound sits above every record the engine writes (an
// artifact is capped at 8 MiB, a probe transcript at 16 MiB). maxRecordServe
// bounds the masked text handed out; a longer one is cut after masking and
// says so.
const (
	maxRecordRead  = 32 << 20
	maxRecordServe = 2 << 20
)

var recordServing sync.Mutex

// boardRow is the board.json row as the ticket page needs it.
type boardRow struct {
	WorkspacePath string          `json:"workspace_path"`
	DeliveryID    string          `json:"delivery_id"`
	IssueKey      string          `json:"issue_key"`
	ClaimedAt     int64           `json:"claimed_at_ms"`
	Raw           json.RawMessage `json:"-"`
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
// wrote are served; an unknown key is a 404, not a directory probe. A key
// that was run more than once has several rows: the newest claim wins,
// whatever order the file lists them in.
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
		public, err := publicBoardRow(entry)
		if err != nil {
			continue
		}
		row.Raw = public
		if !haveRow || row.ClaimedAt > found.ClaimedAt {
			found, haveRow = row, true
		}
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
	// The runner's workspace is supplied by the attendant's canonical
	// Hermes listing, never by a client. Old cards keep their existing
	// directories; observing them must not move or recreate their records.
	if filepath.IsAbs(row.WorkspacePath) && filepath.Clean(row.WorkspacePath) != string(os.PathSeparator) {
		runDir = filepath.Clean(row.WorkspacePath)
	}
	if rest == "live" || strings.HasPrefix(rest, "live/") {
		s.serveTicketLive(w, r, runDir, strings.TrimPrefix(strings.TrimPrefix(rest, "live"), "/"))
		return
	}
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
// name must be one ticketview resolves; the file is read from the run
// directory the board row named, never from a path the client wrote. The
// whole file passes the secret scan before anything is cut (a file too
// large to scan whole is refused), so a value can never be split across
// a cut and leak in halves.
func (s *boardServer) serveTicketRecord(w http.ResponseWriter, r *http.Request, runDir, rest string) {
	// One record at a time: a request reads and scans up to 32 MiB, and
	// the board shares the pod's memory with the engine.
	recordServing.Lock()
	defer recordServing.Unlock()
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
	file, err := os.Open(filepath.Join(runDir, relative))
	if err != nil {
		http.Error(w, "この記録は残っていません", http.StatusNotFound)
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxRecordRead+1))
	if err != nil {
		http.Error(w, "この記録は読めませんでした", http.StatusNotFound)
		return
	}
	if len(raw) > maxRecordRead {
		http.Error(w, "この原本は大きすぎるため画面では出しません (Pod 内で参照してください)", http.StatusRequestEntityTooLarge)
		return
	}
	cut := false
	masked, _, refusal := probe.MaskSecrets(string(raw), nil)
	if refusal != "" {
		http.Error(w, "この記録は秘密の形 ("+refusal+") を含むため表示しません", http.StatusForbidden)
		return
	}
	if len(masked) > maxRecordServe {
		end := maxRecordServe
		for end > 0 && (masked[end]&0xC0) == 0x80 {
			end--
		}
		masked, cut = masked[:end], true
	}
	if cut {
		masked += "\n…(以下略: この原本は長いためここまでで切ってあります)\n"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(masked))
}
