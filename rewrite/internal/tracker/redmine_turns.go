package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// AddComment posts the text once as a note and returns the new note's id.
// Redmine answers a note with no content and no id, so the issue's journals
// are read before and after: the note is the newest one after it was sent, by
// the key's own account, in exactly these words. A failure may still have
// posted it, so nothing here posts it again: read the comments before trying
// again.
func (p RedmineProject) AddComment(ctx context.Context, issue Issue, text string) (int64, error) {
	journal, err := p.Client.addNote(ctx, issue.Key, text)
	if err != nil {
		return 0, err
	}
	id, _, _ := p.CommentText(journal) // addNote read its id
	return id, nil
}

// addNote posts a note and returns its journal record as journals lists it.
func (r Redmine) addNote(ctx context.Context, key, text string) (json.RawMessage, error) {
	if !utf8.ValidString(text) {
		return nil, errors.New("comment contains invalid UTF-8; it was not sent")
	}
	if strings.TrimSpace(text) == "" {
		// Redmine records no note for these words and says nothing of it.
		return nil, errors.New("a comment without words is not recorded by Redmine; it was not sent")
	}
	if _, err := redmineNumber(key); err != nil {
		return nil, err
	}
	me, err := r.myself(ctx)
	if err != nil {
		return nil, err
	}
	before, err := r.journals(ctx, key)
	if err != nil {
		return nil, err
	}
	newest := int64(0)
	for _, raw := range before {
		var journal redmineJournal
		json.Unmarshal(raw, &journal) // journals read every id
		newest = max(newest, journal.ID)
	}
	if _, err := r.call(ctx, http.MethodPut, "/issues/"+key+".json", nil, map[string]any{"issue": map[string]string{"notes": text}},
		redmineItemLimit, http.StatusNoContent, http.StatusOK); err != nil {
		return nil, fmt.Errorf("comment submission not confirmed; it may already be posted; inspect comments before retrying: %w", err)
	}
	after, err := r.journals(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("comment submission not confirmed; it may already be posted; inspect comments before retrying: %w", err)
	}
	var found json.RawMessage
	for _, raw := range after {
		var journal redmineJournal
		if json.Unmarshal(raw, &journal) != nil || journal.ID <= newest || journal.User.ID != me.ID || journal.Notes == nil || *journal.Notes != text {
			continue
		}
		// Two posts of the same words at once by the same account are told
		// apart by nothing; the newer is taken, as the engine takes it.
		found = raw
	}
	if found == nil {
		return nil, errors.New("comment submission returned no readable id; it may already be posted; inspect comments before retrying")
	}
	return found, nil
}

// EditComment replaces the words of a note the key's account posted, through
// the journals API of Redmine 5.0 and later. It needs the role's permission
// to edit its own notes. Redmine answers without the note, so the issue is
// read back for the new words. On an older Redmine, or without the
// permission, the edit is refused and the engine posts a new comment instead.
func (p RedmineProject) EditComment(ctx context.Context, issue Issue, id int64, text string) error {
	if id <= 0 {
		return errors.New("provide a positive comment id")
	}
	if !utf8.ValidString(text) {
		return errors.New("comment contains invalid UTF-8; it was not sent")
	}
	if strings.TrimSpace(text) == "" {
		// Emptying a note deletes it.
		return errors.New("a comment cannot be edited to have no words")
	}
	// A journal's id names no issue; it is edited only when it is this
	// issue's.
	if _, err := p.Client.journal(ctx, issue.Key, id); err != nil {
		return err
	}
	if _, err := p.Client.call(ctx, http.MethodPut, "/journals/"+strconv.FormatInt(id, 10)+".json", nil,
		map[string]any{"journal": map[string]string{"notes": text}}, redmineItemLimit, http.StatusNoContent, http.StatusOK); err != nil {
		return fmt.Errorf("comment edit not confirmed; it may already be applied; read the comment before retrying: %w", err)
	}
	raw, err := p.Client.journal(ctx, issue.Key, id)
	if err != nil {
		return fmt.Errorf("comment edit not confirmed; it may already be applied; read the comment before retrying: %w", err)
	}
	if _, words, _ := p.CommentText(raw); words != text {
		return errors.New("tracker did not confirm the comment edit; read the comment before retrying")
	}
	return nil
}

// journal is one journal of the issue, as journals lists it, or an error
// that reads as not found when the issue has none with that id.
func (r Redmine) journal(ctx context.Context, key string, id int64) (json.RawMessage, error) {
	rows, err := r.journals(ctx, key)
	if err != nil {
		return nil, err
	}
	for _, raw := range rows {
		var journal redmineJournal
		if json.Unmarshal(raw, &journal) == nil && journal.ID == id {
			return raw, nil
		}
	}
	return nil, &trackerError{Status: http.StatusNotFound, Body: "comment not found on the assigned issue"}
}

// Target is the turn's status id as a JSON number. Redmine has no status for
// acceptance of its own here: an issue carries one category, which the
// engine leaves as the requester set it.
func (p RedmineProject) Target(turn string) json.RawMessage {
	id := p.Statuses[turn]
	if turn == Accepted || id <= 0 {
		return nil
	}
	return json.RawMessage(strconv.FormatInt(id, 10))
}

// Move sets the turn's status. Redmine leaves a status it does not allow
// as it was, and answers as though it had changed it, so the issue is read
// back for it.
func (p RedmineProject) Move(ctx context.Context, issue Issue, turn string) error {
	id := p.Statuses[turn]
	if turn == Accepted || id <= 0 {
		return fmt.Errorf("no status is configured for the turn %s", turn)
	}
	return p.Client.change(ctx, issue.Key, map[string]int64{"status_id": id}, func(record redmineIssue) error {
		if record.Status.ID != id {
			return fmt.Errorf("tracker did not confirm the status change: the issue is still in status %d; Redmine leaves the status as it was, without an error, unless the engine's account has Edit issues and its role's workflow allows the move from that status to %d", record.Status.ID, id)
		}
		return nil
	})
}

// Assign hands the issue to the account by its id. Redmine leaves the
// assignee as it was, without an error, when the key's account may not edit
// the issue, so the issue is read back for it.
func (p RedmineProject) Assign(ctx context.Context, issue Issue, to Account) error {
	if to.ID <= 0 {
		return errors.New("provide a positive user id")
	}
	return p.Client.change(ctx, issue.Key, map[string]int64{"assigned_to_id": to.ID}, func(record redmineIssue) error {
		if record.AssignedTo == nil || record.AssignedTo.ID != to.ID {
			return fmt.Errorf("tracker did not confirm the assignee change: Redmine did not assign %d to the issue; it leaves the assignee as it was, without an error, unless the engine's account has Edit issues", to.ID)
		}
		return nil
	})
}

// change sets fields of one issue and has confirm read the issue afterwards.
func (r Redmine) change(ctx context.Context, key string, fields map[string]int64, confirm func(redmineIssue) error) error {
	if _, err := redmineNumber(key); err != nil {
		return err
	}
	if _, err := r.call(ctx, http.MethodPut, "/issues/"+key+".json", nil, map[string]any{"issue": fields},
		redmineItemLimit, http.StatusNoContent, http.StatusOK); err != nil {
		return err
	}
	data, err := r.issue(ctx, key, false)
	if err != nil {
		return fmt.Errorf("the change was sent, and the issue could not be read back: %w", err)
	}
	var record redmineIssue
	if err := json.Unmarshal(data, &record); err != nil {
		return errors.New("the change was sent, and the issue read back could not be read")
	}
	return confirm(record)
}

// RecordHours records nothing: Redmine keeps time as entries that add up, so
// a request worked again would be counted twice. The turn counts as taken,
// so it is not asked again.
func (p RedmineProject) RecordHours(context.Context, Issue, float64) error { return nil }
