package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func scopeList(t *testing.T, source Upstream, ctx context.Context, after, count int64) []json.RawMessage {
	t.Helper()
	body, err := source.Forward(ctx, http.MethodGet, "/issues/7/comments", url.Values{"minId": {fmt.Sprint(after)}, "count": {fmt.Sprint(count)}}, nil, http.StatusOK)
	var rows []json.RawMessage
	if err != nil || json.Unmarshal(body, &rows) != nil {
		t.Fatalf("scoped list: %v", err)
	}
	return rows
}

func TestGitHubEnumerationAllowsConcurrentReaders(t *testing.T) {
	g, _ := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		rows := []any{}
		for id := (page-1)*100 + 1; id <= min(page*100, 205); id++ {
			rows = append(rows, githubScopeComment(base, int64(id), "unchanged"))
		}
		json.NewEncoder(w).Encode(rows)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := g.scopeSource()
	start := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for _, after := range []int64{0, 100, 200} {
				rows := scopeList(t, source, ctx, after, 100)
				if len(rows) != min(100, 205-int(after)) {
					t.Errorf("concurrent read lost rows after %d: %d", after, len(rows))
				}
				for i, raw := range rows {
					var record struct{ ID int64 }
					if json.Unmarshal(raw, &record) != nil || record.ID != after+int64(i)+1 {
						t.Error("concurrent read mixed its cursor with another reader")
					}
				}
			}
		}()
	}
	close(start)
	readers.Wait()
}

func TestGitHubEnumerationRefreshesAfterAnotherOperationOrCursor(t *testing.T) {
	for _, shape := range []string{"new zero", "different count", "different cursor", "post", "read issue", "cancellation"} {
		t.Run(shape, func(t *testing.T) {
			var generation atomic.Int64
			g, calls := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost:
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(githubScopeComment(base, 301, "posted"))
				case r.URL.Path == "/repos/octo-org/widgets/issues/7":
					json.NewEncoder(w).Encode(githubRecord(base, 7))
				default:
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					page = max(page, 1)
					rows := []any{}
					for id := (page-1)*100 + 1; id <= min(page*100, 205); id++ {
						rows = append(rows, githubScopeComment(base, int64(id), fmt.Sprint(generation.Load())))
					}
					json.NewEncoder(w).Encode(rows)
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			source := g.scopeSource()
			if len(scopeList(t, source, ctx, 0, 100)) != 100 {
				t.Fatal("initial page missing")
			}
			generation.Store(1)
			after, count := int64(100), int64(100)
			var err error
			switch shape {
			case "new zero":
				after = 0
			case "different count":
				count = 50
			case "different cursor":
				after = 99
			case "post":
				_, err = source.Forward(ctx, http.MethodPost, "/issues/7/comments", nil, url.Values{"content": {"posted"}}, http.StatusCreated)
			case "read issue":
				_, err = source.Forward(ctx, http.MethodGet, "/issues/7", nil, nil, http.StatusOK)
			case "cancellation":
				stopped, stop := context.WithCancel(ctx)
				stop()
				_, got := source.Forward(stopped, http.MethodGet, "/issues/7/comments", url.Values{"minId": {"100"}}, nil, http.StatusOK)
				if got == nil {
					t.Fatal("canceled enumeration returned a cached answer")
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := calls.Load()
			rows := scopeList(t, source, ctx, after, count)
			if len(rows) != int(count) || calls.Load()-before != 3 {
				t.Fatalf("enumeration was not refreshed: rows=%d calls=%d", len(rows), calls.Load()-before)
			}
			for _, row := range rows {
				var record struct{ Content string }
				if json.Unmarshal(row, &record) != nil || record.Content != "1" {
					t.Fatal("old comment body survived refresh")
				}
			}
		})
	}
}

func TestGitHubScopeDoesNotRetainAnOversizedList(t *testing.T) {
	body := strings.Repeat("x", 90_000)
	g, calls := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		rows := []any{}
		for id := (page-1)*100 + 1; id <= min(page*100, 101); id++ {
			rows = append(rows, githubScopeComment(base, int64(id), body))
		}
		json.NewEncoder(w).Encode(rows)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := g.scopeSource().(*githubScopePages)
	for _, after := range []int64{0, 1} {
		if rows := scopeList(t, source, ctx, after, 1); len(rows) != 1 || source.rows != nil {
			t.Fatal("oversized remote list was retained or refused instead of using ordinary pagination")
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("oversized list unexpectedly reused: calls=%d", calls.Load())
	}
}

func TestGitHubEnumerationDoesNotCrossRoleLaunches(t *testing.T) {
	var generation atomic.Int64
	g, calls := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		rows := []any{}
		for id := (page-1)*100 + 1; id <= min(page*100, 205); id++ {
			rows = append(rows, githubScopeComment(base, int64(id), fmt.Sprint(generation.Load())))
		}
		json.NewEncoder(w).Encode(rows)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first, second := g.scopeSource(), g.scopeSource()
	scopeList(t, first, ctx, 0, 100)
	generation.Store(1)
	// A different role starting at a nonzero cursor must not inherit the first
	// role's unfinished read, even when the requested cursor matches exactly.
	rows := scopeList(t, second, ctx, 100, 100)
	if calls.Load() != 6 {
		t.Fatalf("role borrowed another role's list: %d calls", calls.Load())
	}
	var record struct{ Content string }
	if json.Unmarshal(rows[0], &record) != nil || record.Content != "1" {
		t.Fatal("another role received the first role's old body")
	}
}

func TestGitHubRoleListReadsEachUpstreamPageOnlyOncePerEnumeration(t *testing.T) {
	for _, total := range []int{32, 205, 1000} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			var generation atomic.Int64
			g, calls := githubFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
				page, _ := strconv.Atoi(r.URL.Query().Get("page"))
				page = max(page, 1)
				if r.Method != http.MethodGet || r.URL.Path != "/repos/octo-org/widgets/issues/7/comments" {
					t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
				}
				rows := []any{}
				for id := (page-1)*100 + 1; id <= min(page*100, total); id++ {
					rows = append(rows, githubScopeComment(base, int64(id), fmt.Sprintf("report %d generation %d", id, generation.Load())))
				}
				json.NewEncoder(w).Encode(rows)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			access, err := ServeIssue(ctx, g, "7", false)
			if err != nil {
				t.Fatal(err)
			}
			defer access.Close()
			client, err := CertificateClient(access.Certificate)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			t.Setenv("LIST_TEST_WORKER", access.Key)
			worker := Backlog{BaseURL: access.URL, KeyEnv: "LIST_TEST_WORKER", Client: client}
			for round := int64(0); round < 2; round++ {
				generation.Store(round)
				before := calls.Load()
				rows, err := worker.Comments(ctx, "7", 0)
				if err != nil || len(rows) != total {
					t.Fatalf("incomplete enumeration: count=%d error=%v", len(rows), err)
				}
				for i, raw := range rows {
					var record struct {
						ID      int
						Content string
					}
					if json.Unmarshal(raw, &record) != nil || record.ID != i+1 || record.Content != fmt.Sprintf("report %d generation %d", i+1, round) {
						t.Fatalf("stale or changed record in enumeration %d at %d", round, i)
					}
				}
				if got, want := calls.Load()-before, int32(total/100+1); got != want {
					t.Errorf("one list of %d comments made %d upstream requests; want %d", total, got, want)
				}
			}
		})
	}
}
