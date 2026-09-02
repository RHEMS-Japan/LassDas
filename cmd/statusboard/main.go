// Command statusboard serves the live pipeline board: one page a human
// opens to watch what LassDas is doing RIGHT NOW, fed by the snapshot the
// attendant writes every tick (internal/attendant/status.go) — plus, by
// the operator's explicit decision (2026-09-01, "全権委任"), the three
// requester actions: answering a question, Go, and stop. Every action is
// posted to the TRACKER as the requester's own comment (the requester's
// Backlog API key, provided via secret); the board never writes ledger or
// run state — the attendant detects the comments exactly like hand-written
// ones. Actions are journaled to actions.jsonl for audit and for
// consistent "sent" rendering across browsers.
//
// Fail-closed exposure: the process refuses to start without basic-auth
// credentials of adequate length, so a misconfigured deployment yields no
// server rather than an open or weakly-guarded one. /healthz alone
// answers unauthenticated (probes).
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
)

//go:embed board.html
var boardPage []byte

// Demo skins: static mockups shown to people behind the board's own
// credentials — never a second door. The embedded copy must equal the
// committed mockup under docs/mockups (a test pins that).
//
//go:embed demo/hud.html
var demoHUDPage []byte

var demoPages = map[string][]byte{"hud": demoHUDPage}

func serveDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page, ok := demoPages[strings.TrimPrefix(r.URL.Path, "/demo/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	writePage(w, page)
}

// writePage sends one self-contained HTML page under the board's strict
// policy: inline style and script only, no external loads, no framing.
func writePage(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(page)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "statusboard:", err)
		os.Exit(1)
	}
}

const minPasswordLength = 16

func run() error {
	statusDir := envOr("LASSDAS_STATUS_DIR", "/data/status")
	addr := envOr("LASSDAS_BOARD_ADDR", ":9200")
	user := os.Getenv("LASSDAS_BOARD_USER")
	pass := os.Getenv("LASSDAS_BOARD_PASS")
	if file := os.Getenv("LASSDAS_BOARD_PASS_FILE"); file != "" {
		// The entrypoint moves the password out of the process environment
		// (every card stage spawns from that environment); the file is the
		// trusted copy.
		raw, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("board password file unreadable: %w", err)
		}
		pass = strings.TrimSpace(string(raw))
	}
	if user == "" || len(pass) < minPasswordLength {
		return fmt.Errorf("LASSDAS_BOARD_USER and LASSDAS_BOARD_PASS (>= %d chars) are required (fail-closed)", minPasswordLength)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	poster, trackerBase, err := buildPoster()
	if err != nil {
		return err
	}
	if poster == nil {
		logger.Info("board actions disabled (no requester credential configured)")
	}

	auth := newAuthGate(user, pass, logger)
	board := &boardServer{
		statusDir: statusDir, trackerBase: trackerBase,
		poster: poster, logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// The tracker's webhook lands here — as a BELL, nothing more. The body
	// is discarded unread: a ring can only make the attendant take one
	// rate-limited extra look at the tracker, never inject data. Tracker
	// webhooks cannot carry auth headers, but the URL is ours to register
	// (cmd/setup/backlog.go does exactly that), so the secret rides in the
	// path: without LASSDAS_BOARD_BELL_TOKEN the route does not exist, and
	// a wrong token is a 404 — anonymous callers cannot even ring.
	if bellToken := os.Getenv("LASSDAS_BOARD_BELL_TOKEN"); bellToken != "" {
		wantToken := sha256.Sum256([]byte(bellToken))
		var bellMu sync.Mutex
		var lastBell time.Time
		mux.HandleFunc("/webhook/", func(w http.ResponseWriter, r *http.Request) {
			gotToken := sha256.Sum256([]byte(strings.TrimPrefix(r.URL.Path, "/webhook/")))
			if subtle.ConstantTimeCompare(gotToken[:], wantToken[:]) != 1 {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodPost {
				http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
				return
			}
			bellMu.Lock()
			ring := time.Since(lastBell) >= 5*time.Second
			if ring {
				lastBell = time.Now()
			}
			bellMu.Unlock()
			// Inside the rate gate the body is drained (bounded) so the
			// connection can be reused; outside it nothing is read — a
			// flood costs this process no more than accept + header parse.
			if ring {
				_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
				if err := os.WriteFile(filepath.Join(statusDir, "wakeup"), []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
					logger.Error("bell write failed", "error", err.Error())
				}
			}
			// Always 200: the tracker disables endpoints that keep failing.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	}
	mux.Handle("/", auth.wrap(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writePage(w, boardPage)
	}))
	mux.Handle("/demo/", auth.wrap(serveDemo))
	mux.Handle("/api/board", auth.wrap(board.serveBoard))
	mux.Handle("/api/act", auth.wrap(board.serveAct))
	mux.Handle("/stream", auth.wrap(board.serveStream))

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	logger.Info("statusboard listening", "addr", addr, "status_dir", statusDir,
		"actions_enabled", poster != nil, "bell_armed", os.Getenv("LASSDAS_BOARD_BELL_TOKEN") != "")
	return server.ListenAndServe()
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// buildPoster assembles the requester-credential comment poster. All three
// pieces (origin, space, key) must be present; a partial configuration is
// a refused start, not a silently read-only board.
func buildPoster() (*backlog.Client, string, error) {
	origin := os.Getenv("LASSDAS_BOARD_TRACKER_ORIGIN")
	space := os.Getenv("LASSDAS_BOARD_TRACKER_SPACE")
	key := os.Getenv("LASSDAS_BOARD_TRACKER_KEY")
	if file := os.Getenv("LASSDAS_BOARD_TRACKER_KEY_FILE"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, "", fmt.Errorf("tracker key file unreadable: %w", err)
		}
		key = strings.TrimSpace(string(raw))
	}
	if origin == "" && space == "" && key == "" {
		return nil, "", nil // actions disabled, watching still works
	}
	if origin == "" || space == "" || key == "" {
		return nil, "", errors.New("tracker origin, space and key must be configured together (fail-closed)")
	}
	client, err := backlog.NewClient(backlog.Config{
		SpaceKey: space, Origin: origin, APIKey: key,
		Timeout: 10 * time.Second, MaxResponseBytes: 1024 * 1024,
	}, nil)
	if err != nil {
		return nil, "", err
	}
	base, err := url.Parse(origin)
	if err != nil {
		return nil, "", err
	}
	return client, base.String(), nil
}

// ---- authentication with a brute-force brake ----

type authGate struct {
	userHash, passHash [32]byte
	logger             *slog.Logger

	mu       sync.Mutex
	failures map[string]*failureWindow
}

type failureWindow struct {
	count    int
	until    time.Time
	lastSeen time.Time
}

// maxFailureEntries bounds the map against clients that rotate their
// apparent address; past the cap the whole map resets — a brief brake
// release, never growth without bound.
const maxFailureEntries = 1024

func newAuthGate(user, pass string, logger *slog.Logger) *authGate {
	return &authGate{
		userHash: sha256.Sum256([]byte(user)), passHash: sha256.Sum256([]byte(pass)),
		logger: logger, failures: map[string]*failureWindow{},
	}
}

// trustedProxies parses LASSDAS_BOARD_TRUSTED_PROXY_CIDRS once at start.
var trustedProxies = parseTrustedProxies(os.Getenv("LASSDAS_BOARD_TRUSTED_PROXY_CIDRS"))

func parseTrustedProxies(raw string) []*net.IPNet {
	var networks []*net.IPNet
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			// Loud, not silent: an all-typo list would otherwise fall back
			// to shared-bucket mode while the operator believes it is set.
			fmt.Fprintf(os.Stderr, "statusboard: ignoring invalid trusted proxy CIDR %q\n", entry)
			continue
		}
		networks = append(networks, network)
	}
	return networks
}

// clientIP keys the brute-force brake. X-Forwarded-For is honoured ONLY
// when the direct peer is a configured trusted proxy (the ALB subnets):
// anything inside the cluster — the untrusted card stages included — can
// reach :9200 directly and would otherwise mint a fresh XFF per attempt,
// bypassing the brake and lockout-targeting real users. Without the
// setting every request keys on RemoteAddr; behind the ALB that merges
// all users into one bucket (a 30s shared lockout at worst — the CIDR
// allowlist and the 16+ char password stay the real walls).
func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	peer := net.ParseIP(ip)
	if peer == nil || len(trustedProxies) == 0 {
		return ip
	}
	trusted := false
	for _, network := range trustedProxies {
		if network.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return ip
	}
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if hop := strings.TrimSpace(parts[len(parts)-1]); hop != "" {
			return hop
		}
	}
	return ip
}

func (g *authGate) wrap(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if g.blocked(ip) {
			http.Error(w, "しばらく待ってから再試行してください", http.StatusTooManyRequests)
			return
		}
		gotUser, gotPass, ok := r.BasicAuth()
		if ok {
			hashedUser := sha256.Sum256([]byte(gotUser))
			hashedPass := sha256.Sum256([]byte(gotPass))
			userMatch := subtle.ConstantTimeCompare(hashedUser[:], g.userHash[:])
			passMatch := subtle.ConstantTimeCompare(hashedPass[:], g.passHash[:])
			if userMatch&passMatch == 1 {
				g.clear(ip)
				next(w, r)
				return
			}
			g.recordFailure(ip)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="LassDas board"`)
		http.Error(w, "認証が必要です", http.StatusUnauthorized)
	})
}

func (g *authGate) blocked(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	window, seen := g.failures[ip]
	if !seen {
		return false
	}
	if !window.until.IsZero() && time.Now().After(window.until) {
		delete(g.failures, ip) // served its block; keep the map from growing
		return false
	}
	return time.Now().Before(window.until)
}

func (g *authGate) recordFailure(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.failures) >= maxFailureEntries {
		g.failures = map[string]*failureWindow{}
	}
	window := g.failures[ip]
	if window == nil {
		window = &failureWindow{}
		g.failures[ip] = window
	}
	// A stale sub-threshold window restarts instead of accumulating
	// forever.
	if time.Since(window.lastSeen) > 10*time.Minute {
		window.count = 0
	}
	window.lastSeen = time.Now()
	window.count++
	if window.count >= 5 {
		window.until = time.Now().Add(30 * time.Second)
		window.count = 0
	}
	g.logger.Info("auth failure", "ip", ip)
}

func (g *authGate) clear(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, ip)
}

// ---- the board server ----

type boardServer struct {
	statusDir   string
	trackerBase string
	poster      *backlog.Client
	logger      *slog.Logger

	actMu   sync.Mutex
	lastAct map[string]time.Time
}

type streamPayload struct {
	Board          json.RawMessage   `json:"board"`
	Events         []json.RawMessage `json:"events"`
	Actions        []json.RawMessage `json:"actions"`
	TrackerBase    string            `json:"tracker_base,omitempty"`
	ActionsEnabled bool              `json:"actions_enabled"`
	SentAt         time.Time         `json:"sent_at"`
}

func (s *boardServer) payload() streamPayload {
	payload := streamPayload{
		Board:          json.RawMessage(`{"schema_version":1,"runs":[]}`),
		Events:         tailJSONL(filepath.Join(s.statusDir, "events.jsonl"), 80),
		Actions:        tailJSONL(filepath.Join(s.statusDir, "actions.jsonl"), 50),
		TrackerBase:    s.trackerBase,
		ActionsEnabled: s.poster != nil,
		SentAt:         time.Now().UTC(),
	}
	if raw, err := os.ReadFile(filepath.Join(s.statusDir, "board.json")); err == nil && json.Valid(raw) {
		payload.Board = raw
	}
	return payload
}

func (s *boardServer) serveBoard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(s.payload())
}

// tailJSONL returns the last limit valid lines, oldest first. The files
// are append-only and unbounded, so only the final chunk is read — never
// the whole file — and every line is validated so a torn write (or the
// partial first line of the chunk) never reaches a client.
func tailJSONL(path string, limit int) []json.RawMessage {
	const tailBytes = 256 << 10
	records := []json.RawMessage{}
	file, err := os.Open(path)
	if err != nil {
		return records
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return records
	}
	offset := int64(0)
	if info.Size() > tailBytes {
		offset = info.Size() - tailBytes
	}
	raw := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(raw, offset); err != nil {
		return records
	}
	if offset > 0 {
		// Drop the (likely partial) first line of the chunk.
		if cut := strings.IndexByte(string(raw), '\n'); cut >= 0 {
			raw = raw[cut+1:]
		}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, line := range lines {
		if line == "" || !json.Valid([]byte(line)) {
			continue
		}
		records = append(records, json.RawMessage(line))
	}
	return records
}

// ---- requester actions ----

type actRequest struct {
	Action  string `json:"action"` // answer | go | stop
	IssueID int64  `json:"issue_id"`
	Text    string `json:"text,omitempty"`
}

// boardRun is the slice of the snapshot the authorization needs.
type boardRun struct {
	DeliveryID string `json:"delivery_id"`
	IssueID    int64  `json:"issue_id"`
	Step       string `json:"step"`
	CanGo      bool   `json:"can_go"`
}

// authorizeAction is the confused-deputy gate: the requester's key posts
// ONLY to an issue the pipeline currently owns, and only in the one state
// where the action is guaranteed to be honoured (CanGo = the posted
// staging report armed the Go anchor; a stop shares the same window —
// that is where the reception loop consumes it).
func (s *boardServer) authorizeAction(action string, issueID int64) (boardRun, string) {
	raw, err := os.ReadFile(filepath.Join(s.statusDir, "board.json"))
	if err != nil {
		return boardRun{}, "盤面の状態を読めないため、操作を受け付けられません"
	}
	var board struct {
		Runs []boardRun `json:"runs"`
	}
	if json.Unmarshal(raw, &board) != nil {
		return boardRun{}, "盤面の状態を読めないため、操作を受け付けられません"
	}
	for _, run := range board.Runs {
		if run.IssueID != issueID {
			continue
		}
		// Allow-list, never pass-through: an action this gate does not
		// know is an action it refuses.
		if (action == "go" || action == "stop") && run.CanGo {
			return run, ""
		}
		return boardRun{}, "この依頼はいま Go / 停止を受け付けられる状態ではありません"
	}
	return boardRun{}, "盤面に無いチケットへの操作は受け付けられません"
}

type actRecord struct {
	At         time.Time `json:"at"`
	Action     string    `json:"action"`
	IssueID    int64     `json:"issue_id"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	// Who: the authenticated board user and the client address, because
	// these actions carry the requester's authority and the audit trail
	// must show the path they took.
	User     string `json:"user,omitempty"`
	ClientIP string `json:"client_ip,omitempty"`
}

// serveAct posts one requester action to the tracker. CSRF is closed two
// ways browsers cannot cross: a strict JSON content type (HTML forms
// cannot send it) and an Origin check when the header is present.
func (s *boardServer) serveAct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
		return
	}
	if s.poster == nil {
		http.Error(w, "このボードは閲覧専用に設定されています", http.StatusForbidden)
		return
	}
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		http.Error(w, "JSON のみ", http.StatusUnsupportedMediaType)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host {
			http.Error(w, "送信元が不正です", http.StatusForbidden)
			return
		}
	}
	var request actRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil || request.IssueID <= 0 {
		http.Error(w, "リクエストが不正です", http.StatusBadRequest)
		return
	}
	var content string
	switch request.Action {
	case "answer":
		// Phase 2 (issue #14): the intake's answer grammar is not carried
		// to the board yet — free text would post but never be adopted,
		// and a button that does nothing is the one thing this board must
		// never show.
		http.Error(w, "回答のボード送信は未開放です (チケットで回答してください)", http.StatusForbidden)
		return
	case "go":
		// Only the first non-blank line decides a Go; the second line keeps
		// the audit trail honest about the path it took.
		content = "Go\n(状態ボードから送信)"
	case "stop":
		content = "停止\n(状態ボードから送信)"
	default:
		http.Error(w, "不明な操作です", http.StatusBadRequest)
		return
	}
	run, denied := s.authorizeAction(request.Action, request.IssueID)
	if denied != "" {
		http.Error(w, denied, http.StatusForbidden)
		return
	}
	throttleKey := fmt.Sprintf("%s:%d", request.Action, request.IssueID)
	if !s.admitAction(throttleKey) {
		http.Error(w, "同じ操作を連続送信しています。数秒待ってください", http.StatusTooManyRequests)
		return
	}
	if _, err := s.poster.AddComment(r.Context(), request.IssueID, content); err != nil {
		s.clearAction(throttleKey) // a failed post must not cost the retry window
		s.logger.Error("action post failed", "action", request.Action, "issue", request.IssueID, "error", err.Error())
		http.Error(w, "チケットへの投稿に失敗しました。時間をおいて再試行してください", http.StatusBadGateway)
		return
	}
	boardUser, _, _ := r.BasicAuth()
	record := actRecord{At: time.Now().UTC(), Action: request.Action, IssueID: request.IssueID, DeliveryID: run.DeliveryID, User: boardUser, ClientIP: clientIP(r)}
	s.journalAction(record)
	s.logger.Info("action posted", "action", request.Action, "issue", request.IssueID, "user", boardUser, "client_ip", record.ClientIP)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(record)
}

func (s *boardServer) clearAction(key string) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	delete(s.lastAct, key)
}

func (s *boardServer) admitAction(key string) bool {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if s.lastAct == nil {
		s.lastAct = map[string]time.Time{}
	}
	if last, seen := s.lastAct[key]; seen && time.Since(last) < 5*time.Second {
		return false
	}
	s.lastAct[key] = time.Now()
	return true
}

// journalAction appends to actions.jsonl — the board's only file, disjoint
// from the attendant's board.json/events.jsonl, so the single-writer rule
// holds per file.
func (s *boardServer) journalAction(record actRecord) {
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(s.statusDir, "actions.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.logger.Error("action journal failed", "error", err.Error())
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.Write(append(encoded, '\n'))
}

// ---- the live feed ----

// serveStream pushes the full payload whenever board.json or actions.jsonl
// changes (1s watch), plus a keep-alive while nothing does.
func (s *boardServer) serveStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	watched := []string{filepath.Join(s.statusDir, "board.json"), filepath.Join(s.statusDir, "actions.jsonl")}
	stamp := func() string {
		var parts []string
		for _, path := range watched {
			if info, err := os.Stat(path); err == nil {
				parts = append(parts, fmt.Sprintf("%d:%d", info.ModTime().UnixNano(), info.Size()))
			} else {
				parts = append(parts, "-")
			}
		}
		return strings.Join(parts, "|")
	}
	send := func() {
		encoded, err := json.Marshal(s.payload())
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: board\ndata: %s\n\n", encoded)
		flusher.Flush()
	}

	send()
	last := stamp()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	heartbeat := 0
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if current := stamp(); current != last {
				last = current
				send()
				heartbeat = 0
				continue
			}
			if heartbeat++; heartbeat >= 15 {
				heartbeat = 0
				if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}
}
