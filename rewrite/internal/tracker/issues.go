package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// Issues reads one explicitly configured project's native issue records.
// It does not assess readiness, claim work or change the tracker. Original
// descriptions and unknown API metadata are preserved for the intake caller.
func (b Backlog) Issues(ctx context.Context, projectID int64) ([]json.RawMessage, error) {
	if projectID <= 0 {
		return nil, errors.New("provide a positive project id for issue discovery")
	}
	issues := []json.RawMessage{}
	seen := map[int64]bool{}
	for offset := 0; ; {
		query := url.Values{"projectId[]": {strconv.FormatInt(projectID, 10)}, "count": {"100"},
			"offset": {strconv.Itoa(offset)}, "sort": {"created"}, "order": {"asc"}}
		data, err := b.call(ctx, http.MethodGet, "/issues", query, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil || page == nil || len(page) > 100 {
			return nil, errors.New("tracker issue page is not a bounded issue array")
		}
		for _, raw := range page {
			var issue struct {
				ID        int64  `json:"id"`
				Key       string `json:"issueKey"`
				ProjectID int64  `json:"projectId"`
			}
			if err := json.Unmarshal(raw, &issue); err != nil || issue.ID <= 0 || issue.Key == "" {
				return nil, errors.New("tracker issue has no readable identity")
			}
			if issue.ProjectID != projectID {
				return nil, errors.New("tracker returned an issue outside the configured project")
			}
			if seen[issue.ID] {
				return nil, errors.New("tracker issue pagination repeated an issue; no partial list returned")
			}
			seen[issue.ID] = true
			issues = append(issues, raw)
		}
		if len(page) < 100 {
			return issues, nil
		}
		offset += len(page)
	}
}
