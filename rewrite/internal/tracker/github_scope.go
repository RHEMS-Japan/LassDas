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

var _ Tracker = GitHub{}

// Forward translates the issue scope's existing API into GitHub requests.
// Roles keep the same CLI and field names; their text is never rewritten.
// IssueScope checks the role's authority before calling this method. DELETE
// is for the scope's own cleanup only, never a role's request.
func (g GitHub) Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if !strings.HasPrefix(path, "/issues/") || len(parts) < 2 {
		return nil, errors.New("outside the issue scope's tracker operations")
	}
	issue := Issue{Key: parts[1]}
	upstream, err := g.issuePath(issue.Key)
	if err != nil {
		return nil, err
	}
	switch {
	case method == http.MethodGet && len(parts) == 2 && len(query) == 0 && len(form) == 0 && expected == http.StatusOK:
		data, _, err := g.call(ctx, method, g.base()+upstream, nil, http.StatusOK, githubItemLimit)
		if err != nil {
			return nil, err
		}
		record, err := g.ReadIssue(data)
		if err != nil || record.Key != issue.Key {
			return nil, errors.New("tracker did not return the assigned issue")
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		fields["id"], fields["issueKey"] = record.ID, record.Key
		fields["summary"], fields["description"] = fields["title"], fields["body"]
		fields["created"], fields["createdUser"] = fields["created_at"], scopedGitHubUser(record.Creator)
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
		// GitHub pages by position, not comment id. Read its complete list
		// before selecting the scope's page, so an unreadable later page does
		// not turn into a successful partial answer. Conditional reads remain
		// shared with the engine's other readers through g.call.
		rows, err := g.Comments(ctx, issue)
		if err != nil {
			return nil, err
		}
		page := []json.RawMessage{}
		for _, raw := range rows {
			comment, err := g.ReadComment(raw, issue)
			if err != nil {
				return nil, err
			}
			if !comment.OnIssue {
				return nil, &githubError{Status: http.StatusNotFound, Body: "comment not found on the assigned issue"}
			}
			if comment.ID > after && int64(len(page)) < count {
				data, err := g.scopedComment(raw, issue, comment.ID)
				if err != nil {
					return nil, err
				}
				page = append(page, data)
			}
		}
		return json.Marshal(page)
	case method == http.MethodPost && len(parts) == 3 && parts[2] == "comments" && len(query) == 0 && len(form) == 1 && len(form["content"]) == 1 && expected == http.StatusCreated:
		data, _, err := g.call(ctx, method, g.base()+upstream+"/comments", map[string]string{"body": form.Get("content")}, http.StatusCreated, githubItemLimit)
		if err != nil {
			return nil, err
		}
		return g.scopedComment(data, issue, 0)
	case (method == http.MethodGet || method == http.MethodDelete) && len(parts) == 4 && parts[2] == "comments" && len(query) == 0 && len(form) == 0 && expected == http.StatusOK:
		id, err := strconv.ParseInt(parts[3], 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != parts[3] {
			return nil, errors.New("provide one positive comment id")
		}
		repository, _ := g.repository() // issuePath already checked it.
		address := g.base() + "/repos/" + repository + "/issues/comments/" + parts[3]
		// GitHub addresses a comment without its issue number. Read the
		// association before returning it or deleting it for this launch.
		data, _, err := g.call(ctx, http.MethodGet, address, nil, http.StatusOK, githubItemLimit)
		if err != nil {
			return nil, err
		}
		normalized, err := g.scopedComment(data, issue, id)
		if err != nil {
			return nil, err
		}
		if method == http.MethodDelete {
			if _, _, err := g.call(ctx, method, address, nil, http.StatusNoContent, githubItemLimit); err != nil {
				return nil, err
			}
			return []byte(`{}`), nil
		}
		return normalized, nil
	default:
		return nil, fmt.Errorf("outside the issue scope's tracker operations: %s", method)
	}
}

func scopedGitHubUser(account Account) map[string]any {
	return map[string]any{"id": account.ID, "userId": account.Login, "name": account.Login}
}

func (g GitHub) scopedComment(raw []byte, issue Issue, id int64) ([]byte, error) {
	comment, err := g.ReadComment(raw, issue)
	if err != nil {
		return nil, err
	}
	if !comment.OnIssue || id != 0 && comment.ID != id {
		return nil, &githubError{Status: http.StatusNotFound, Body: "comment not found on the assigned issue"}
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields["id"], fields["content"] = comment.ID, comment.Body
	fields["created"], fields["createdUser"] = fields["created_at"], scopedGitHubUser(comment.Author)
	return json.Marshal(fields)
}
