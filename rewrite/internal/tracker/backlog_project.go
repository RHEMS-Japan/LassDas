package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// BacklogProject is one Backlog project as the engine's tracker. The ids are
// the operator's, from the engine's intake settings: which status means what,
// and which category marks what, is theirs, and nothing here names one.
type BacklogProject struct {
	Client    Backlog
	ProjectID int64
	// Categories narrows the intake to issues carrying one of them; none
	// takes up every issue.
	Categories []int64
	// OnAccept is the category added to an issue the engine accepted, zero
	// for none.
	OnAccept int64
	// Statuses names, by status id, where the issue moves at each turn of the
	// work; a turn without one leaves the status alone.
	Statuses map[string]int64
}

var _ Tracker = BacklogProject{}

func (p BacklogProject) Identity() string {
	return fmt.Sprintf("Issue intake: %s\nProject: %d", p.Client.BaseURL, p.ProjectID)
}

func (p BacklogProject) CredentialEnv() string { return p.Client.KeyEnv }

func (p BacklogProject) Issues(ctx context.Context) ([]json.RawMessage, error) {
	return p.Client.Issues(ctx, p.ProjectID)
}

// backlogIssue is the part of an issue record the engine reads.
type backlogIssue struct {
	ID        int64                `json:"id"`
	Key       string               `json:"issueKey"`
	ProjectID int64                `json:"projectId"`
	Created   time.Time            `json:"created"`
	Creator   struct{ ID int64 }   `json:"createdUser"`
	Category  []struct{ ID int64 } `json:"category"`
}

func (p BacklogProject) ReadIssue(raw json.RawMessage) (Issue, error) {
	var record backlogIssue
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 || record.Key == "" || record.ProjectID != p.ProjectID {
		return Issue{}, errors.New("the issue record could not be read for the configured project")
	}
	return Issue{ID: record.ID, Key: record.Key, Created: record.Created, Creator: Account{ID: record.Creator.ID}, Raw: raw}, nil
}

// categories are the ids of the categories the issue's record carries.
func (p BacklogProject) categories(issue Issue) []int64 {
	var record backlogIssue
	if len(issue.Raw) == 0 || json.Unmarshal(issue.Raw, &record) != nil {
		return nil
	}
	ids := []int64{}
	for _, category := range record.Category {
		ids = append(ids, category.ID)
	}
	return ids
}

func (p BacklogProject) Marked(issue Issue) bool {
	if len(p.Categories) == 0 {
		return true
	}
	for _, category := range p.categories(issue) {
		for _, id := range p.Categories {
			if category == id {
				return true
			}
		}
	}
	return false
}

func (p BacklogProject) Request(ctx context.Context, key string) (string, error) {
	return p.Client.Request(ctx, key)
}

func (p BacklogProject) RequestText(raw json.RawMessage) (string, error) {
	return RequestText(raw)
}

func (p BacklogProject) Comments(ctx context.Context, issue Issue) ([]json.RawMessage, error) {
	return p.Client.Comments(ctx, issue.Key, 0)
}

// ReadComment places a comment by the issue and the project its record names.
func (p BacklogProject) ReadComment(raw json.RawMessage, issue Issue) (Comment, error) {
	var record struct {
		ID, IssueID, ProjectID int64
		Content                string
		CreatedUser            struct{ ID int64 }
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return Comment{}, errors.New("the comment record could not be read")
	}
	return Comment{ID: record.ID, Body: record.Content, Author: Account{ID: record.CreatedUser.ID},
		OnIssue: record.IssueID == issue.ID && record.ProjectID == p.ProjectID}, nil
}

func (p BacklogProject) CommentText(raw json.RawMessage) (int64, string, error) {
	var record struct {
		ID      int64
		Content string
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return 0, "", errors.New("the comment record could not be read")
	}
	return record.ID, record.Content, nil
}

func (p BacklogProject) AddComment(ctx context.Context, issue Issue, text string) (int64, error) {
	receipt, err := p.Client.AddComment(ctx, issue.Key, text)
	if err != nil {
		return 0, err
	}
	// The receipt was read for a positive id before it was returned.
	var stored struct{ ID int64 }
	json.Unmarshal(receipt, &stored)
	return stored.ID, nil
}

func (p BacklogProject) Myself(ctx context.Context) (Account, error) {
	id, err := p.Client.Myself(ctx)
	if err != nil {
		return Account{}, err
	}
	return Account{ID: id}, nil
}

// Target is the turn's status id, or for an accepted issue the category
// added, as a JSON number.
func (p BacklogProject) Target(turn string) json.RawMessage {
	id := p.Statuses[turn]
	if turn == Accepted {
		id = p.OnAccept
	}
	if id <= 0 {
		return nil
	}
	return json.RawMessage(strconv.FormatInt(id, 10))
}

// Move sets the turn's status. An accepted issue keeps the categories it
// carries and gains the configured one, since the change replaces the whole
// list.
func (p BacklogProject) Move(ctx context.Context, issue Issue, turn string) error {
	if turn != Accepted {
		return p.Client.SetStatus(ctx, issue.Key, p.Statuses[turn])
	}
	ids := []int64{}
	for _, id := range p.categories(issue) {
		if id > 0 && id != p.OnAccept {
			ids = append(ids, id)
		}
	}
	return p.Client.SetCategories(ctx, issue.Key, append(ids, p.OnAccept))
}

func (p BacklogProject) Assign(ctx context.Context, issue Issue, to Account) error {
	return p.Client.SetAssignee(ctx, issue.Key, to.ID)
}

func (p BacklogProject) RecordHours(ctx context.Context, issue Issue, hours float64) error {
	return p.Client.SetActualHours(ctx, issue.Key, hours)
}

func (p BacklogProject) Forward(ctx context.Context, method, path string, query, form url.Values, expected int) ([]byte, error) {
	return p.Client.call(ctx, method, path, query, form, expected)
}
