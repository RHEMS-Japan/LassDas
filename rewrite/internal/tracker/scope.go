package tracker

import (
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

// IssueScope exposes only one operator-assigned issue to a worker. The upstream
// account credential stays with this handler, outside the worker environment.
// Its key is an API capability, not a certificate attached to an LLM's answer.
// The caller must supply TLS and isolate the handler from worker processes.
type IssueScope struct {
	source Backlog
	issue  string
	key    string
	post   bool
	// latest makes a later post through this scope replace its earlier one.
	latest bool
	mu     sync.Mutex
	posted int64
}

// KeepLatestPost makes a scope leave one comment: when it posts again, the
// comment it posted before is removed once the new one is stored. A scope
// lives for one launch, so a role that posts a trial line before what it
// means to say leaves only the latter. Nothing is read to decide which: the
// later post stays. Removing the earlier one is best effort; a refusal
// leaves both, which is what there was before this option.
func KeepLatestPost(s *IssueScope) { s.latest = true }

func NewIssueScope(source Backlog, issue string, mayPost bool, options ...func(*IssueScope)) (*IssueScope, error) {
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
	// Backlog.call inserts only the upstream account credential and refuses
	// redirects. It sends a POST once; a missing receipt stays ambiguous.
	data, err := s.source.call(r.Context(), r.Method, requested, query, form, expected)
	if err != nil {
		s.reject(w, http.StatusBadGateway, "scoped tracker request: "+err.Error())
		return
	}
	if r.Method == http.MethodPost && s.latest {
		s.replaceEarlierPost(r, path, data)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(expected)
	w.Write(data)
}

// replaceEarlierPost removes the comment this scope stored before the one the
// tracker has just accepted. The new comment is stored first, so a failure
// here can only leave one comment too many, never none.
func (s *IssueScope) replaceEarlierPost(r *http.Request, path string, receipt []byte) {
	var stored struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(receipt, &stored) != nil || stored.ID <= 0 {
		return
	}
	s.mu.Lock()
	earlier := s.posted
	s.posted = stored.ID
	s.mu.Unlock()
	if earlier > 0 && earlier != stored.ID {
		s.source.call(r.Context(), http.MethodDelete, path+"/comments/"+strconv.FormatInt(earlier, 10), nil, nil, http.StatusOK)
	}
}

func (s *IssueScope) reject(w http.ResponseWriter, status int, message string) {
	if s.key != "" {
		message = strings.ReplaceAll(message, s.key, "[access key]")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]string{"message": message}}})
}
