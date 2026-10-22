package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"ticket-runner/internal/tracker"
)

type seenComment struct {
	ID      int64     `json:"id"`
	Author  int64     `json:"author_id"`
	SHA256  string    `json:"sha256"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

type recordingTracker struct {
	tracker.Tracker
	jobs string
}

// Preserve the optional editing capability used by notice settlement.
type recordingCommentEditor struct {
	*recordingTracker
	commentEditor
}

func recordTrackerComments(source tracker.Tracker, jobs string) tracker.Tracker {
	recorder := &recordingTracker{Tracker: source, jobs: jobs}
	if editor, ok := source.(commentEditor); ok {
		return recordingCommentEditor{recorder, editor}
	}
	return recorder
}

func (s *recordingTracker) Comments(ctx context.Context, issue tracker.Issue) ([]json.RawMessage, error) {
	rows, err := s.Tracker.Comments(ctx, issue)
	if err != nil || issue.ID == 0 {
		return rows, err
	}
	comments := make([]tracker.Comment, 0, len(rows))
	for _, raw := range rows {
		comment, err := s.Tracker.ReadComment(raw, issue)
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	if err := s.record(issue, comments); err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *recordingTracker) ReadComment(raw json.RawMessage, issue tracker.Issue) (tracker.Comment, error) {
	comment, err := s.Tracker.ReadComment(raw, issue)
	if err != nil {
		return comment, err
	}
	if err := s.record(issue, []tracker.Comment{comment}); err != nil {
		return tracker.Comment{}, err
	}
	return comment, nil
}

// cfg.source makes fresh wrappers. Share a mutex for each queue and issue id
// across those wrappers, including the watcher and run goroutines.
var commentsSeenLocks sync.Map

func (s *recordingTracker) record(issue tracker.Issue, comments []tracker.Comment) error {
	if issue.ID == 0 {
		return nil
	}
	pending := make([]seenComment, 0, len(comments))
	for _, comment := range comments {
		if comment.Body == "" {
			continue
		}
		digest := sha256.Sum256([]byte(comment.Body))
		pending = append(pending, seenComment{ID: comment.ID, Author: comment.Author.ID,
			SHA256: hex.EncodeToString(digest[:]), Created: comment.Created, Updated: comment.Updated})
	}
	if len(pending) == 0 {
		return nil
	}
	if s.jobs == "" {
		return errors.New("comment recording has no jobs directory")
	}
	directory, err := filepath.Abs(filepath.Join(s.jobs, strconv.FormatInt(issue.ID, 10)))
	if err != nil {
		return err
	}
	lock, _ := commentsSeenLocks.LoadOrStore(directory, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	path := filepath.Join(directory, "comments-seen.json")
	var seen []seenComment
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading seen comments: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &seen); err != nil {
			return errors.New("the seen comments are unreadable; the record was not replaced")
		}
	}
	known := make(map[int64]bool, len(seen))
	for _, comment := range seen {
		known[comment.ID] = true
	}
	changed := false
	for _, comment := range pending {
		if !known[comment.ID] {
			seen = append(seen, comment)
			known[comment.ID] = true
			changed = true
		}
	}
	if !changed {
		return nil
	}
	data, err = json.Marshal(seen)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := writeRuntimeFile(path, data); err != nil {
		return fmt.Errorf("writing seen comments: %w", err)
	}
	return nil
}
