package tracker

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Upstream carries out a request that an issue scope let through. It adds only
// the account's own credential, follows no redirect, and sends a POST once: a
// missing receipt stays ambiguous.
type Upstream interface {
	Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error)
}

// Forward is Backlog's guarded call, for an issue scope in front of it.
func (b Backlog) Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	return b.call(ctx, method, path, query, form, expected)
}

// IssueScope exposes only one operator-assigned issue to a worker. The upstream
// account credential stays with this handler, outside the worker environment.
// Its key is an API capability, not a certificate attached to an LLM's answer.
// The caller must supply TLS and isolate the handler from worker processes.
type IssueScope struct {
	source Upstream
	issue  string
	key    string
	post   bool
	// latest makes the scope leave only the last comment it stored.
	latest bool
	mu     sync.Mutex
	posts  []int64
	stored func(int64) error
}

// KeepLatestPost makes a scope leave one comment. The scope lives for one
// launch and remembers the comments stored through it; when the launch is
// over, RemoveEarlierPosts removes all of them but the one stored last (the
// highest id the tracker gave). A role that posts a trial line before what it
// means to say therefore leaves only the latter. Nothing is read to decide
// which: the last stored stays. The removal happens after the role's posts
// were answered, never in their path, and is best effort: a comment the
// tracker will not remove stays, which is what there was before this option.
func KeepLatestPost(s *IssueScope) { s.latest = true }

// ObserveStoredPost records a successful POST's native comment id before its
// receipt reaches the worker. It observes transport, not the comment's words.
func ObserveStoredPost(record func(int64) error) func(*IssueScope) {
	return func(s *IssueScope) { s.stored = record }
}

func NewIssueScope(source Upstream, issue string, mayPost bool, options ...func(*IssueScope)) (*IssueScope, error) {
	if issue == "" || issue == "." || issue == ".." || strings.ContainsAny(issue, "/\\?#\r\n\x00") {
		return nil, errors.New("provide one operator-assigned tracker issue")
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, fmt.Errorf("creating issue-scoped access: %w", err)
	}
	scope := &IssueScope{source: source, issue: issue, key: base64.RawURLEncoding.EncodeToString(key[:]), post: mayPost}
	for _, option := range options {
		option(scope)
	}
	return scope, nil
}

// Key grants only this instance's issue access; do not log it or save it in a
// shared workspace. Closing the serving runtime revokes that runtime's access.
func (s *IssueScope) Key() string { return s.key }

func (s *IssueScope) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.reject(w, http.StatusBadRequest, "unreadable tracker parameters")
		return
	}
	keys := query["apiKey"]
	if s.key == "" || len(keys) != 1 || subtle.ConstantTimeCompare([]byte(keys[0]), []byte(s.key)) != 1 {
		s.reject(w, http.StatusUnauthorized, "issue-scoped access is unavailable")
		return
	}
	delete(query, "apiKey")
	path := "/issues/" + url.PathEscape(s.issue)
	requested := r.URL.EscapedPath()
	var form url.Values
	expected := http.StatusOK
	switch {
	case r.Method == http.MethodGet && requested == path:
		if len(query) != 0 {
			s.reject(w, 400, "unexpected issue parameters")
			return
		}
	case r.Method == http.MethodGet && requested == path+"/comments":
		for name, values := range query {
			if len(values) != 1 {
				s.reject(w, 400, "ambiguous comment parameters")
				return
			}
			switch name {
			case "order":
				if values[0] != "asc" {
					s.reject(w, 400, "use ascending comment order")
					return
				}
			case "count", "minId":
				n, err := strconv.ParseInt(values[0], 10, 64)
				if err != nil || n < 0 || (name == "count" && (n < 1 || n > 100)) {
					s.reject(w, 400, "invalid comment pagination")
					return
				}
			default:
				s.reject(w, 400, "unexpected comment parameters")
				return
			}
		}
	case r.Method == http.MethodGet && strings.HasPrefix(requested, path+"/comments/"):
		id := strings.TrimPrefix(requested, path+"/comments/")
		n, err := strconv.ParseInt(id, 10, 64)
		if len(query) != 0 || err != nil || n <= 0 || strconv.FormatInt(n, 10) != id {
			s.reject(w, 400, "provide one positive comment id")
			return
		}
	case r.Method == http.MethodPost && requested == path+"/comments" && s.post:
		if len(query) != 0 {
			s.reject(w, 400, "unexpected post parameters")
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/x-www-form-urlencoded" {
			s.reject(w, http.StatusUnsupportedMediaType, "use a comment form")
			return
		}
		// Bound transport resources, not the meaning or format of a report.
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var sizeError *http.MaxBytesError
			if errors.As(err, &sizeError) {
				s.reject(w, http.StatusRequestEntityTooLarge, "comment form exceeds 4 MiB")
			} else {
				s.reject(w, http.StatusBadRequest, "reading comment form: "+err.Error())
			}
			return
		}
		form, err = url.ParseQuery(string(body))
		if err != nil || len(form) != 1 || len(form["content"]) != 1 {
			s.reject(w, 400, "only one comment content field is available")
			return
		}
		expected = http.StatusCreated
	default:
		s.reject(w, http.StatusForbidden, "outside this issue's granted tracker operations")
		return
	}
	// Never forward caller authority, paths, hosts or notification/status fields.
	// Forward inserts only the upstream account credential and refuses
	// redirects. It sends a POST once; a missing receipt stays ambiguous.
	data, err := s.source.Forward(r.Context(), r.Method, requested, query, form, expected)
	if err != nil {
		status := http.StatusBadGateway
		var missing *trackerError
		if errors.As(err, &missing) && missing.Status == http.StatusNotFound {
			status = http.StatusNotFound
		}
		s.reject(w, status, "scoped tracker request: "+err.Error())
		return
	}
	if r.Method == http.MethodPost && (s.latest || s.stored != nil) {
		if err := s.remember(data); err != nil {
			s.reject(w, http.StatusBadGateway, "comment was submitted but its receipt could not be recorded: "+err.Error())
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(expected)
	w.Write(data)
}

// remember notes the comment the tracker has just stored for this scope. The
// id comes from the tracker's own receipt, never from the worker's request.
func (s *IssueScope) remember(receipt []byte) error {
	var stored struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(receipt, &stored) != nil || stored.ID <= 0 {
		if s.stored != nil {
			return errors.New("tracker receipt has no positive comment id")
		}
		return nil
	}
	s.mu.Lock()
	s.posts = append(s.posts, stored.ID)
	s.mu.Unlock()
	if s.stored != nil {
		return s.stored(stored.ID)
	}
	return nil
}

// RemoveEarlierPosts removes every comment this scope stored except the one
// stored last, and is a no-op without KeepLatestPost. Call it once the launch
// is over. It returns how many comments it removed.
func (s *IssueScope) RemoveEarlierPosts(ctx context.Context) int {
	s.mu.Lock()
	posts := s.posts
	latest := int64(0)
	for _, id := range posts {
		if id > latest {
			latest = id
		}
	}
	if latest > 0 {
		s.posts = []int64{latest}
	}
	s.mu.Unlock()
	if !s.latest {
		return 0
	}
	removed := 0
	path := "/issues/" + url.PathEscape(s.issue)
	for _, id := range posts {
		if id == latest {
			continue
		}
		if _, err := s.source.Forward(ctx, http.MethodDelete, path+"/comments/"+strconv.FormatInt(id, 10), nil, nil, http.StatusOK); err == nil {
			removed++
		}
	}
	return removed
}

func (s *IssueScope) reject(w http.ResponseWriter, status int, message string) {
	if s.key != "" {
		message = strings.ReplaceAll(message, s.key, "[access key]")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]string{"message": message}}})
}
