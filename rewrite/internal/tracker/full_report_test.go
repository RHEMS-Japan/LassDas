package tracker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLongReportKeepsEveryFindingAndTheFinalExamples(t *testing.T) {
	var report strings.Builder
	for n := 1; n <= 20; n++ {
		fmt.Fprintf(&report, "所見 %d: %s\r\n", n, strings.Repeat("調査の根拠と確認結果。", 35))
	}
	report.WriteString("設定例: {\"enabled\":true}\n確認: curl -I https://service.example.invalid/\n")
	prose := report.String()
	for _, kind := range []string{"backlog", "github"} {
		t.Run(kind, func(t *testing.T) {
			var worker Backlog
			issue, id := "EXAMPLE-1", int64(7)
			if kind == "github" {
				issue, id = "7", 41
				_, worker, _, _ = githubScopeFixture(t, func(base string, _ int32, w http.ResponseWriter, r *http.Request) {
					switch r.Method + " " + r.URL.Path {
					case "POST /repos/octo-org/widgets/issues/7/comments":
						var body map[string]string
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["body"] != prose {
							t.Errorf("upstream report changed: %v, got %d bytes, want %d", err, len(body["body"]), len(prose))
						}
						w.WriteHeader(http.StatusCreated)
					case "GET /repos/octo-org/widgets/issues/comments/41":
					default:
						t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
					}
					json.NewEncoder(w).Encode(githubScopeComment(base, id, prose))
				}, true)
			} else {
				worker, _ = scopedFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.Method + " " + r.URL.Path {
					case "POST /api/v2/issues/EXAMPLE-1/comments":
						if err := r.ParseForm(); err != nil || r.PostForm.Get("content") != prose {
							t.Errorf("upstream report changed: %v, got %d bytes, want %d", err, len(r.PostForm.Get("content")), len(prose))
						}
						w.WriteHeader(http.StatusCreated)
					case "GET /api/v2/issues/EXAMPLE-1/comments/7":
					default:
						t.Errorf("unexpected operation: %s %s", r.Method, r.URL.Path)
					}
					json.NewEncoder(w).Encode(map[string]any{"id": id, "content": prose})
				}), true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			posted, err := worker.AddComment(ctx, issue, prose)
			if err != nil {
				t.Fatal(err)
			}
			read, err := worker.Comment(ctx, issue, id)
			if err != nil {
				t.Fatal(err)
			}
			for _, data := range [][]byte{posted, read} {
				var result struct{ Content string }
				if err := json.Unmarshal(data, &result); err != nil || result.Content != prose {
					t.Fatalf("report did not come back whole: %v, got %d bytes, want %d", err, len(result.Content), len(prose))
				}
			}
		})
	}
}
