package tracker

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Redmine sets no limit of its own on how often it is asked, so the bounds
// are the engine's: the size of one answer (redmine_test.go) and the number
// of pages one list may take. A server that keeps saying there is more is not
// read forever.
func TestRedmineReadsNoMoreThanItsBoundOfPages(t *testing.T) {
	fake, redmine := redmineFixture(t)
	pages := 0
	fake.before = func(w http.ResponseWriter, r *http.Request) bool {
		pages++
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		page := make([]any, 100)
		for i := range page {
			page[i] = map[string]any{"id": offset + i + 1, "project": map[string]any{"id": 1}}
		}
		json.NewEncoder(w).Encode(map[string]any{"issues": page, "total_count": 1 << 30})
		return true
	}
	rows, err := redmine.Issues(context.Background())
	if err == nil || rows != nil || !strings.Contains(err.Error(), "ran past 1000 pages") || pages != redminePages {
		t.Fatalf("pages=%d rows=%d error=%v", pages, len(rows), err)
	}
}
