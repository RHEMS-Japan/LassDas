package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Forward translates the issue scope's existing API into Redmine requests.
// Roles keep the same CLI and field names; their text is never rewritten.
// IssueScope checks the role's authority before calling this method. DELETE
// is for the scope's own cleanup only, never a role's request: it empties
// the note, which Redmine 5.0 and later takes as removing it.
func (p RedmineProject) Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	r := p.Client
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !strings.HasPrefix(path, "/issues/") || len(parts) < 2 {
		return nil, errors.New("outside the issue scope's tracker operations")
	}
	key := parts[1]
	if _, err := redmineNumber(key); err != nil {
		return nil, err
	}
	switch {
	case method == http.MethodGet && len(parts) == 2 && len(query) == 0 && len(form) == 0 && expected == http.StatusOK:
		data, err := r.issue(ctx, key, false)
		if err != nil {
			return nil, err
		}
		var issue redmineIssue
		var fields map[string]any
		if err := json.Unmarshal(data, &issue); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		fields["issueKey"], fields["summary"], fields["description"] = key, issue.Subject, issue.Description
		fields["created"], fields["createdUser"] = fields["created_on"], scopedRedmineUser(issue.Author)
		return json.Marshal(fields)
	case method == http.MethodGet && len(parts) == 3 && parts[2] == "comments" && len(form) == 0 && expected == http.StatusOK:
		after, count := int64(0), int64(100)
		for name, values := range query {
			if len(values) != 1 {
				return nil, errors.New("ambiguous comment parameters")
			}
			if name == "order" && values[0] == "asc" {
				continue
			}
			n, err := strconv.ParseInt(values[0], 10, 64)
			if err != nil || n < 0 || (name != "count" && name != "minId") || name == "count" && (n < 1 || n > 100) {
				return nil, errors.New("invalid comment pagination")
			}
			if name == "minId" {
				after = n
			} else {
				count = n
			}
		}
		// Redmine gives every journal of an issue in one read; the scope's page
		// is chosen from it.
		rows, err := r.journals(ctx, key)
		if err != nil {
			return nil, err
		}
		page := []json.RawMessage{}
		for _, raw := range rows {
			var journal redmineJournal
			json.Unmarshal(raw, &journal) // journals read every id
			if journal.ID > after && int64(len(page)) < count {
				data, err := scopedRedmineJournal(raw)
				if err != nil {
					return nil, err
				}
				page = append(page, data)
			}
		}
		return json.Marshal(page)
	case method == http.MethodPost && len(parts) == 3 && parts[2] == "comments" && len(query) == 0 && len(form) == 1 && len(form["content"]) == 1 && expected == http.StatusCreated:
		raw, err := r.addNote(ctx, key, form.Get("content"))
		if err != nil {
			return nil, err
		}
		return scopedRedmineJournal(raw)
	case (method == http.MethodGet || method == http.MethodDelete) && len(parts) == 4 && parts[2] == "comments" && len(query) == 0 && len(form) == 0 && expected == http.StatusOK:
		id, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != parts[3] {
			return nil, errors.New("provide one positive comment id")
		}
		// A journal's id names no issue. It is found among this issue's
		// before it is returned or emptied for this launch.
		raw, err := r.journal(ctx, key, id)
		if err != nil {
			return nil, err
		}
		if method == http.MethodDelete {
			if _, err := r.call(ctx, http.MethodPut, "/journals/"+parts[3]+".json", nil, map[string]any{"journal": map[string]string{"notes": ""}},
				redmineItemLimit, http.StatusNoContent, http.StatusOK); err != nil {
				return nil, err
			}
			return []byte(`{}`), nil
		}
		return scopedRedmineJournal(raw)
	default:
		return nil, fmt.Errorf("outside the issue scope's tracker operations: %s", method)
	}
}

func scopedRedmineUser(user redmineReference) map[string]any {
	return map[string]any{"id": user.ID, "name": user.Name}
}

// scopedRedmineJournal is a journal with the scope's field names added: its
// words as content, its time as created and its author as createdUser.
func scopedRedmineJournal(raw json.RawMessage) ([]byte, error) {
	var journal struct {
		redmineJournal
		CreatedOn any `json:"created_on"`
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &journal); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	content := ""
	if journal.Notes != nil {
		content = *journal.Notes
	}
	fields["content"], fields["created"], fields["createdUser"] = content, journal.CreatedOn, scopedRedmineUser(journal.User)
	return json.Marshal(fields)
}
