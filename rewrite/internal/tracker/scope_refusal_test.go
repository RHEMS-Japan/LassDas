package tracker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestIssueScopeMissingResponsesAreIndependentOfTracker(t *testing.T) {
	for _, kind := range []string{"github", "backlog"} {
		for _, status := range []int{400, 401, 403, 404, 422, 429, 500, 503} {
			t.Run(fmt.Sprintf("%s/%d", kind, status), func(t *testing.T) {
				var scope *IssueScope
				calls := 0
				secret := "synthetic-account+/=key"
				if kind == "github" {
					secret = githubTestToken
					g, _ := githubFixture(t, func(_ string, _ int32, w http.ResponseWriter, r *http.Request) {
						calls++
						w.WriteHeader(status)
						fmt.Fprint(w, "upstream detail "+secret)
					})
					var err error
					scope, err = NewIssueScope(g, "7", false)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					_, scope = scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						w.WriteHeader(status)
						fmt.Fprint(w, "upstream detail "+secret)
					}), false)
				}
				issue := "7"
				if kind == "backlog" {
					issue = "EXAMPLE-1"
				}
				r := httptest.NewRequest(http.MethodGet, "/issues/"+issue+"/comments/41?apiKey="+scope.Key(), nil)
				w := httptest.NewRecorder()
				scope.ServeHTTP(w, r)
				want := http.StatusBadGateway
				if status == http.StatusNotFound {
					want = http.StatusNotFound
				}
				text := w.Body.String()
				if w.Code != want || calls != 1 || !strings.Contains(text, fmt.Sprintf("HTTP %d: upstream detail", status)) || strings.Contains(text, secret) {
					t.Fatalf("refusal was reclassified, hidden, leaked or retried: status=%d calls=%d text=%s", w.Code, calls, text)
				}
			})
		}
	}
}

type wrappedScopeRefusal struct{}

func (wrappedScopeRefusal) Forward(context.Context, string, string, url.Values, url.Values, int) ([]byte, error) {
	return nil, fmt.Errorf("reading the assigned comment: %w", &trackerError{Status: http.StatusNotFound, Body: "missing"})
}

func TestIssueScopeRecognizesAWrappedCommonRefusal(t *testing.T) {
	scope, err := NewIssueScope(wrappedScopeRefusal{}, "7", false)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/issues/7/comments/41?apiKey="+scope.Key(), nil)
	w := httptest.NewRecorder()
	scope.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "reading the assigned comment") {
		t.Fatalf("wrapped common refusal lost: %d %s", w.Code, w.Body)
	}
}
