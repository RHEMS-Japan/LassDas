package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestGitHubScopedPageNeverExceedsTheRequestedCount(t *testing.T) {
	g, _ := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		rows := []any{}
		for id := (page-1)*100 + 1; id <= min(page*100, 205); id++ {
			rows = append(rows, githubScopeComment(base, int64(id), fmt.Sprint(id)))
		}
		json.NewEncoder(w).Encode(rows)
	})
	for _, count := range []int{1, 2, 99, 100} {
		for _, after := range []int{0, 1, 100, 204, 205} {
			t.Run(fmt.Sprintf("count=%d/after=%d", count, after), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				query := url.Values{"count": {strconv.Itoa(count)}, "minId": {strconv.Itoa(after)}, "order": {"asc"}}
				body, err := g.Forward(ctx, http.MethodGet, "/issues/7/comments", query, nil, http.StatusOK)
				var rows []struct{ ID int64 }
				want := min(count, 205-after)
				if err != nil || json.Unmarshal(body, &rows) != nil || len(rows) != want {
					t.Fatalf("scoped page length=%d want=%d error=%v", len(rows), want, err)
				}
				for i, row := range rows {
					if row.ID != int64(after+i+1) {
						t.Fatalf("scoped page skipped or repeated a comment: %+v", rows)
					}
				}
			})
		}
	}
}

type scopeErrorSource struct{ reason string }

func (s *scopeErrorSource) Forward(context.Context, string, string, url.Values, url.Values, int) ([]byte, error) {
	return nil, errors.New(s.reason)
}

func TestIssueScopeScrubsItsKeyFromTheRawUpstreamFailureResponse(t *testing.T) {
	source := &scopeErrorSource{}
	scope, err := NewIssueScope(source, "7", false)
	if err != nil {
		t.Fatal(err)
	}
	source.reason = "upstream refused " + scope.Key() + "; repeated " + scope.Key()
	request := httptest.NewRequest(http.MethodGet, "https://tracker.example/issues/7?"+url.Values{"apiKey": {scope.Key()}}.Encode(), nil)
	reply := httptest.NewRecorder()
	scope.ServeHTTP(reply, request)
	text := reply.Body.String()
	if reply.Code != http.StatusBadGateway || strings.Contains(text, scope.Key()) || strings.Count(text, "[access key]") != 2 || !strings.Contains(text, "upstream refused") {
		t.Fatalf("scope response lost the reason or did not scrub both occurrences (status=%d)", reply.Code)
	}
}
