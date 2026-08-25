package githubapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// A candidate's commit may add files, not only modify them: RFDEV-622's
// delivery (a SQL migration plus three new modules) passed every review and
// the deterministic validation, then died here because the verifier accepted
// only "modified". Anything else — removals, renames — stays a refusal, since
// a candidate cannot express those.
func TestVerifyChangedPathsAcceptsAddedFiles(t *testing.T) {
	shaX := strings.Repeat("c", 40)
	shaY := strings.Repeat("d", 40)
	compare := "/repos/example/consumer/compare/" + shaX + "..." + shaY
	cases := []struct {
		name   string
		files  string
		wantOK bool
	}{
		{"modified and added", `[{"filename":"api/a.ts","status":"modified"},{"filename":"api/b.sql","status":"added"}]`, true},
		{"removed refused", `[{"filename":"api/a.ts","status":"removed"},{"filename":"api/b.sql","status":"added"}]`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			controller, _ := newTestController(t, []requestStep{
				{method: http.MethodGet, path: compare,
					body: `{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"files":` + testCase.files + `}`},
			}, true)
			err := controller.verifyChangedPaths(context.Background(), shaX, shaY, []string{"api/a.ts", "api/b.sql"})
			if (err == nil) != testCase.wantOK {
				t.Fatalf("verifyChangedPaths() error = %v, wantOK %v", err, testCase.wantOK)
			}
		})
	}
}
