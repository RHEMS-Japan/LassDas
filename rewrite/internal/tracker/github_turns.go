package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// GitHubLabels names, per turn of the work, the label an issue is given. A
// turn without one leaves the labels alone. The labels must exist in the
// repository; nothing here creates one.
type GitHubLabels struct {
	Accepted          string `json:"accepted,omitempty"`
	Processing        string `json:"processing,omitempty"`
	AwaitingRequester string `json:"awaiting_requester,omitempty"`
	Delivered         string `json:"delivered,omitempty"`
	Stopped           string `json:"stopped,omitempty"`
}

func (l GitHubLabels) label(turn string) string {
	switch turn {
	case Accepted:
		return l.Accepted
	case Processing:
		return l.Processing
	case AwaitingRequester:
		return l.AwaitingRequester
	case Delivered:
		return l.Delivered
	case Stopped:
		return l.Stopped
	}
	return ""
}

// AddComment posts the text once. A failure may still have posted it, so
// nothing here posts it again: read the comments before trying again.
func (g GitHub) AddComment(ctx context.Context, issue Issue, text string) (int64, error) {
	path, err := g.issuePath(issue.Key)
	if err != nil {
		return 0, err
	}
	data, _, err := g.call(ctx, http.MethodPost, g.base()+path+"/comments", map[string]string{"body": text}, http.StatusCreated, githubItemLimit)
	if err != nil {
		return 0, fmt.Errorf("comment submission not confirmed; it may already be posted; inspect comments before retrying: %w", err)
	}
	var receipt struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.ID <= 0 {
		return 0, errors.New("comment submission returned no readable id; it may already be posted; inspect comments before retrying")
	}
	return receipt.ID, nil
}

// Target is the turn's label name, as a JSON string.
func (g GitHub) Target(turn string) json.RawMessage {
	name := g.Labels.label(turn)
	if name == "" {
		return nil
	}
	target, _ := json.Marshal(name)
	return target
}

// Move gives the issue the turn's label. A working turn (processing, awaiting
// the requester, delivered, stopped) also takes off the label of any other
// working turn the issue carries, so the issue shows one; the label of
// acceptance stays, as do the labels people gave the issue. A label already
// gone counts as taken off, and a move cut short is completed by asking for
// it again.
func (g GitHub) Move(ctx context.Context, issue Issue, turn string) error {
	name := g.Labels.label(turn)
	if name == "" {
		return fmt.Errorf("no label is configured for the turn %s", turn)
	}
	path, err := g.issuePath(issue.Key)
	if err != nil {
		return err
	}
	// The answer lists every label the issue carries afterwards.
	data, _, err := g.call(ctx, http.MethodPost, g.base()+path+"/labels", map[string][]string{"labels": {name}}, http.StatusOK, githubItemLimit)
	if err != nil {
		return err
	}
	var carried []githubLabel
	if err := json.Unmarshal(data, &carried); err != nil || !hasLabel(carried, name) {
		return errors.New("tracker did not confirm the label")
	}
	if turn == Accepted {
		return nil
	}
	for _, other := range []string{g.Labels.Processing, g.Labels.AwaitingRequester, g.Labels.Delivered, g.Labels.Stopped} {
		if other == "" || strings.EqualFold(other, name) || !hasLabel(carried, other) {
			continue
		}
		_, _, err := g.call(ctx, http.MethodDelete, g.base()+path+"/labels/"+url.PathEscape(other), nil, http.StatusOK, githubItemLimit)
		var refusal *githubError
		if err != nil && !(errors.As(err, &refusal) && refusal.Status == http.StatusNotFound) {
			return err
		}
	}
	return nil
}

func hasLabel(labels []githubLabel, name string) bool {
	for _, label := range labels {
		if strings.EqualFold(label.Name, name) {
			return true
		}
	}
	return false
}

// Assign hands the issue to the account by its login. GitHub adds an assignee
// without replacing the others, and ignores without a word one it will not
// assign, so the answer is read for the account. The other side of the
// hand-over is then taken off: the requester when the engine's own account
// takes the issue, the engine's account when the requester does. Anyone
// else assigned stays.
func (g GitHub) Assign(ctx context.Context, issue Issue, to Account) error {
	if to.Login == "" {
		return errors.New("GitHub assigns by login, and the account has none")
	}
	path, err := g.issuePath(issue.Key)
	if err != nil {
		return err
	}
	assignees := func(data []byte) ([]githubUser, error) {
		var updated struct {
			Assignees []githubUser `json:"assignees"`
		}
		err := json.Unmarshal(data, &updated)
		return updated.Assignees, err
	}
	data, _, err := g.call(ctx, http.MethodPost, g.base()+path+"/assignees", map[string][]string{"assignees": {to.Login}}, http.StatusCreated, githubItemLimit)
	if err != nil {
		return err
	}
	current, err := assignees(data)
	if err != nil || !hasLogin(current, to.Login) {
		return errors.New("tracker did not confirm the assignee change")
	}
	me, err := g.Myself(ctx)
	if err != nil {
		return err
	}
	other := ""
	switch {
	case strings.EqualFold(to.Login, me.Login):
		other = issue.Creator.Login
	case strings.EqualFold(to.Login, issue.Creator.Login):
		other = me.Login
	}
	if other == "" || strings.EqualFold(other, to.Login) || !hasLogin(current, other) {
		return nil
	}
	data, _, err = g.call(ctx, http.MethodDelete, g.base()+path+"/assignees", map[string][]string{"assignees": {other}}, http.StatusOK, githubItemLimit)
	if err != nil {
		return err
	}
	if current, err = assignees(data); err != nil || hasLogin(current, other) {
		return errors.New("tracker did not confirm the assignee change")
	}
	return nil
}

func hasLogin(users []githubUser, login string) bool {
	for _, user := range users {
		if strings.EqualFold(user.Login, login) {
			return true
		}
	}
	return false
}

// RecordHours records nothing: GitHub has no field for the hours a piece of
// work took. The turn counts as taken, so it is not asked again.
func (g GitHub) RecordHours(context.Context, Issue, float64) error { return nil }
