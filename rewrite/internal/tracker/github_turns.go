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
// acceptance stays, as do the labels people gave the issue. A label is taken
// off under the name GitHub gave it, and one GitHub answers it cannot find
// counts as taken off only when the issue's labels, read again, no longer
// hold it. A move cut short is completed by asking for it again.
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
		given, found := labelNamed(carried, other)
		if other == "" || strings.EqualFold(other, name) || !found {
			continue
		}
		_, _, err := g.call(ctx, http.MethodDelete, g.base()+path+"/labels/"+url.PathEscape(given), nil, http.StatusOK, githubItemLimit)
		var refusal *githubError
		if err == nil {
			continue
		}
		if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
			return err
		}
		rows, err := g.pages(ctx, path+"/labels?per_page=100")
		if err != nil {
			return fmt.Errorf("the label %q was answered as not found, and the issue's labels could not be read again: %w", given, err)
		}
		var now []githubLabel
		for _, raw := range rows {
			var label githubLabel
			if json.Unmarshal(raw, &label) != nil {
				return errors.New("the issue's labels could not be read again")
			}
			now = append(now, label)
		}
		if _, still := labelNamed(now, given); still {
			return fmt.Errorf("the label %q could not be taken off: GitHub answered that it was not found, and the issue still carries it", given)
		}
	}
	return nil
}

func hasLabel(labels []githubLabel, name string) bool {
	_, found := labelNamed(labels, name)
	return found
}

// labelNamed is a label's name as the issue carries it, matched without
// regard to case.
func labelNamed(labels []githubLabel, name string) (string, bool) {
	for _, label := range labels {
		if strings.EqualFold(label.Name, name) {
			return label.Name, true
		}
	}
	return "", false
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
		return fmt.Errorf("tracker did not confirm the assignee change: GitHub did not assign %q to the issue; it assigns an account with access to the repository or one that commented on the issue, by its current login, and no more than ten to one issue", to.Login)
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
		return fmt.Errorf("tracker did not confirm the assignee change: GitHub did not take %q off the issue; it takes an assignee off only for an account with push access to the repository", other)
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
