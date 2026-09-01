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

// The promotion gate must accept the same statuses the candidate gate does:
// after RFDEV-622 taught verifyChangedPaths to accept "added", verifyPathSet
// still rejected it, so any file-creating delivery cleared publish and
// staging only to die at promotion. Deletions stay refusals on both gates.
func TestVerifyPathSetAcceptsAddedFilesLikeTheCandidateGate(t *testing.T) {
	baseSHA := strings.Repeat("c", 40)
	headSHA := strings.Repeat("d", 40)
	baseTree := strings.Repeat("e", 40)
	compare := "/repos/example/consumer/compare/" + baseSHA + "..." + headSHA
	cases := []struct {
		name   string
		files  []comparisonFile
		wantOK bool
	}{
		{"modified and added", []comparisonFile{{Filename: "api/a.ts", Status: "modified"}, {Filename: "api/b.sql", Status: "added"}}, true},
		{"removed refused", []comparisonFile{{Filename: "api/a.ts", Status: "removed"}, {Filename: "api/b.sql", Status: "added"}}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			controller, _ := newTestController(t, []requestStep{
				{method: http.MethodGet, path: compare,
					body: compareJSON("ahead", baseSHA, baseTree, baseSHA, baseTree, testCase.files)},
			}, true)
			err := controller.verifyPathSet(context.Background(), baseSHA, baseTree, headSHA, []string{"api/a.ts", "api/b.sql"}, baseSHA, baseTree)
			if (err == nil) != testCase.wantOK {
				t.Fatalf("verifyPathSet() error = %v, wantOK %v", err, testCase.wantOK)
			}
		})
	}
}
