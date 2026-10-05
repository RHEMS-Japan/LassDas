package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// One ordinary enumeration starts at zero and follows the IDs returned by the
// preceding page. Retain its already-validated remote list instead of fetching
// every remote page again for every local page. A new zero cursor, a different
// cursor/count, any other operation, or completion discards it. Concurrent or
// interleaved enumerations may refetch; no consistency across them is promised.
// Standalone nonzero cursors and lists over 8 MiB use the uncached path.
type githubScopePages struct {
	GitHub
	mu           sync.Mutex
	rows         []json.RawMessage
	issue        string
	after, count int64
}

const githubScopeListLimit = 8 << 20

func (g GitHub) scopeSource() Upstream { return &githubScopePages{GitHub: g} }

func (s *githubScopePages) clear() { s.rows, s.issue, s.after, s.count = nil, "", 0, 0 }

func (s *githubScopePages) Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		s.clear()
		return nil, err
	}
	if method != http.MethodGet || !strings.HasSuffix(path, "/comments") {
		s.clear()
	}
	data, err := s.GitHub.forward(ctx, method, path, query, form, expected, s.comments)
	if err != nil {
		s.clear()
	}
	return data, err
}

func (s *githubScopePages) comments(ctx context.Context, issue Issue, after, count int64) ([]json.RawMessage, error) {
	rows := s.rows
	if rows == nil || after == 0 || issue.Key != s.issue || after != s.after || count != s.count {
		s.clear()
		var err error
		rows, err = s.GitHub.Comments(ctx, issue)
		if err != nil {
			return nil, err
		}
		size := 0
		for _, raw := range rows {
			comment, err := s.ReadComment(raw, issue)
			if err != nil {
				return nil, err
			}
			if !comment.OnIssue {
				return nil, &githubError{Status: http.StatusNotFound, Body: "comment not found on the assigned issue"}
			}
			size += len(raw)
		}
		if after == 0 && size <= githubScopeListLimit {
			s.rows, s.issue, s.count = rows, issue.Key, count
		}
	}
	if s.rows != nil {
		returned := int64(0)
		for _, raw := range rows {
			comment, _ := s.ReadComment(raw, issue) // the complete list was validated above
			if comment.ID > after && returned < count {
				s.after = comment.ID
				returned++
			}
		}
		if returned < count {
			s.clear()
		}
	}
	return rows, nil
}
