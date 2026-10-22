package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/tracker"
)

type seenTestTracker struct {
	tracker.Tracker
	rows []json.RawMessage
	err  error
}

func (s seenTestTracker) Comments(context.Context, tracker.Issue) ([]json.RawMessage, error) {
	return s.rows, s.err
}

type seenTestEditor struct {
	seenTestTracker
	edited func(context.Context, tracker.Issue, int64, string) error
}

func (s seenTestEditor) EditComment(ctx context.Context, issue tracker.Issue, id int64, text string) error {
	return s.edited(ctx, issue, id, text)
}

func seenTestRaw(id, author int64, body string, hour int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":%d,"issueId":51,"projectId":17,"content":%q,"createdUser":{"id":%d},"created":"2026-01-03T%02d:00:00Z","updated":"2026-01-03T%02d:00:00Z"}`, id, body, author, hour, hour))
}

func readSeenTest(t *testing.T, jobs string) []seenComment {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(jobs, "51", "comments-seen.json"))
	if err != nil {
		t.Fatal(err)
	}
	var seen []seenComment
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatal(err)
	}
	return seen
}

func TestCommentsSeenKeepsFirstReadAcrossWrappers(t *testing.T) {
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.MkdirAll(filepath.Join(jobs, "51"), 0700); err != nil {
		t.Fatal(err)
	}
	issue := tracker.Issue{ID: 51, Key: "EXAMPLE-51"}
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}, rows: []json.RawMessage{
		seenTestRaw(701, 55, "original 日本語", 0), seenTestRaw(702, 56, "", 0),
	}}
	source := recordTrackerComments(base, jobs)
	rows, err := source.Comments(context.Background(), issue)
	if err != nil || !reflect.DeepEqual(rows, base.rows) {
		t.Fatalf("comments changed: %s (%v)", rows, err)
	}
	first := readSeenTest(t, jobs)
	digest := sha256.Sum256([]byte("original 日本語"))
	want := seenComment{ID: 701, Author: 55, SHA256: hex.EncodeToString(digest[:]), Created: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Updated: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)}
	if len(first) != 1 || first[0] != want {
		t.Fatalf("first: %+v", first)
	}
	base.rows = []json.RawMessage{seenTestRaw(701, 77, "edited", 1), seenTestRaw(703, 55, "new comment", 1)}
	// A fresh wrapper represents another cfg.source call or a restarted run.
	source = recordTrackerComments(base, jobs)
	if _, err := source.Comments(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadComment(seenTestRaw(701, 88, "edited again", 2), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadComment(seenTestRaw(704, 55, "single read", 2), issue); err != nil {
		t.Fatal(err)
	}
	if _, err := source.ReadComment(seenTestRaw(705, 55, "", 2), issue); err != nil {
		t.Fatal(err)
	}
	seen := readSeenTest(t, jobs)
	if len(seen) != 3 || seen[0] != want || seen[1].ID != 703 || seen[2].ID != 704 {
		t.Fatalf("later reads: %+v", seen)
	}
	// A later empty list never removes old entries.
	base.rows = nil
	if _, err := recordTrackerComments(base, jobs).Comments(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
	if got := readSeenTest(t, jobs); !reflect.DeepEqual(got, seen) {
		t.Fatalf("entries removed: %+v", got)
	}
}

func TestCommentsSeenSkipsEmptyBodiesAndUnknownIssue(t *testing.T) {
	for _, id := range []int64{0, 51} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			jobs := filepath.Join(t.TempDir(), "jobs")
			body := ""
			if id == 0 {
				body = "not empty"
			}
			base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}, rows: []json.RawMessage{seenTestRaw(701, 55, body, 0)}}
			source := recordTrackerComments(base, jobs)
			issue := tracker.Issue{ID: id, Key: "EXAMPLE-51"}
			if _, err := source.Comments(context.Background(), issue); err != nil {
				t.Fatal(err)
			}
			if _, err := source.ReadComment(base.rows[0], issue); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(jobs); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("created jobs: %v", err)
			}
		})
	}
}

func TestCommentsSeenConcurrentReadersKeepEveryRow(t *testing.T) {
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.MkdirAll(filepath.Join(jobs, "51"), 0700); err != nil {
		t.Fatal(err)
	}
	issue := tracker.Issue{ID: 51, Key: "EXAMPLE-51"}
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}}
	for id := int64(1); id <= 40; id++ {
		base.rows = append(base.rows, seenTestRaw(id, 55, "list", 0))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			source := recordTrackerComments(base, jobs)
			<-start
			for id := int64(41); id <= 80; id++ {
				if worker == 0 {
					if _, err := source.Comments(context.Background(), issue); err != nil {
						t.Error(err)
						return
					}
				} else {
					if _, err := source.ReadComment(seenTestRaw(id, 56, "single", 0), issue); err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
	seen := readSeenTest(t, jobs)
	ids := map[int64]bool{}
	for _, row := range seen {
		ids[row.ID] = true
	}
	if len(seen) != 80 || len(ids) != 80 {
		t.Fatalf("lost or duplicated rows: %d rows, %d ids", len(seen), len(ids))
	}
	for id := int64(1); id <= 80; id++ {
		if !ids[id] {
			t.Errorf("missing %d", id)
		}
	}
}

func TestCommentsSeenPreservesEditingAndDelegates(t *testing.T) {
	issue := tracker.Issue{ID: 51, Key: "EXAMPLE-51"}
	ctx := context.WithValue(context.Background(), struct{}{}, "example")
	failure := errors.New("edit refused")
	called := false
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}}
	source := recordTrackerComments(seenTestEditor{seenTestTracker: base, edited: func(got context.Context, target tracker.Issue, id int64, text string) error {
		called = true
		if got != ctx || !reflect.DeepEqual(target, issue) || id != 701 || text != "replacement" {
			t.Fatal("edit arguments changed")
		}
		return failure
	}}, t.TempDir())
	editor, ok := source.(commentEditor)
	if !ok {
		t.Fatal("editing capability lost")
	}
	if err := editor.EditComment(ctx, issue, 701, "replacement"); err != failure || !called {
		t.Fatalf("edit not delegated: %v", err)
	}
	if source.Identity() != base.Identity() {
		t.Fatal("identity changed")
	}
	if _, ok := recordTrackerComments(base, t.TempDir()).(commentEditor); ok {
		t.Fatal("editing capability invented")
	}
}

func TestSourceCommentRecordingFlag(t *testing.T) {
	for _, cfg := range []config{{}, {GitHub: &tracker.GitHub{}}, {Redmine: &tracker.Redmine{}}} {
		original := cfg.unrecordedSource()
		if got := cfg.source(); reflect.TypeOf(got) != reflect.TypeOf(original) || !reflect.DeepEqual(got, original) {
			t.Fatalf("flag off changed %T", got)
		}
		cfg.recordComments = true
		cfg.commentJobs = filepath.Join(t.TempDir(), "jobs")
		if got := cfg.source(); reflect.TypeOf(got) == reflect.TypeOf(original) {
			t.Fatalf("flag on did not wrap %T", got)
		}
	}
	cfg := config{recordComments: true, commentJobs: filepath.Join(t.TempDir(), "jobs"), Intake: &intakeConfig{ProjectID: 17}}
	if err := os.MkdirAll(filepath.Join(cfg.commentJobs, "51"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.source().ReadComment(seenTestRaw(701, 55, "source read", 0), tracker.Issue{ID: 51}); err != nil {
		t.Fatal(err)
	}
	if got := readSeenTest(t, cfg.commentJobs); len(got) != 1 || got[0].ID != 701 {
		t.Fatalf("source record: %+v", got)
	}
	for _, raw := range []string{`{"recordComments":true}`, `{"commentJobs":"somewhere"}`} {
		if _, err := readConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted internal setting: %s", raw)
		}
	}
}

func TestCommentsSeenRefusesUnreadableRecordsAndStorageFailures(t *testing.T) {
	issue := tracker.Issue{ID: 51}
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}, rows: []json.RawMessage{seenTestRaw(701, 55, "words", 0)}}
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprint(damaged), func(t *testing.T) {
			jobs := t.TempDir()
			path := filepath.Join(jobs, "51")
			if damaged {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(path, "comments-seen.json")
			}
			original := []byte("unreadable")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			source := recordTrackerComments(base, jobs)
			if _, err := source.Comments(context.Background(), issue); err == nil {
				t.Fatal("list hid storage failure")
			}
			if _, err := source.ReadComment(base.rows[0], issue); err == nil {
				t.Fatal("single read hid storage failure")
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, original) {
				t.Fatal("damaged record replaced")
			}
		})
	}
	base.err = errors.New("tracker unavailable")
	if _, err := recordTrackerComments(base, t.TempDir()).Comments(context.Background(), issue); err != base.err {
		t.Fatalf("tracker error changed: %v", err)
	}
	if _, err := recordTrackerComments(base, t.TempDir()).ReadComment(json.RawMessage(`{"id":0}`), issue); err == nil {
		t.Fatal("invalid comment accepted")
	}
}

func TestCommentsSeenPassesThroughUnreadableAndOffIssueRows(t *testing.T) {
	jobs := t.TempDir()
	if err := os.Mkdir(filepath.Join(jobs, "51"), 0700); err != nil {
		t.Fatal(err)
	}
	issue := tracker.Issue{ID: 51}
	odd := json.RawMessage(`{"id":950,"issueId":"51","projectId":null,"createdUser":"someone","content":"unrelated words"}`)
	offIssue := json.RawMessage(`{"id":951,"issueId":52,"projectId":17,"createdUser":{"id":55},"content":"another issue"}`)
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}, rows: []json.RawMessage{
		odd, offIssue, seenTestRaw(952, 55, "readable words", 0),
	}}
	rows, err := recordTrackerComments(base, jobs).Comments(context.Background(), issue)
	if err != nil || !reflect.DeepEqual(rows, base.rows) {
		t.Fatalf("comments changed: %s (%v)", rows, err)
	}
	if seen := readSeenTest(t, jobs); len(seen) != 1 || seen[0].ID != 952 {
		t.Fatalf("recorded unreadable or off-issue rows: %+v", seen)
	}
}

func TestCommentsSeenDoesNotCreateMissingJobDirectory(t *testing.T) {
	jobs := filepath.Join(t.TempDir(), "jobs")
	issue := tracker.Issue{ID: 51}
	base := seenTestTracker{Tracker: tracker.BacklogProject{ProjectID: 17}, rows: []json.RawMessage{seenTestRaw(701, 55, "words", 0)}}
	source := recordTrackerComments(base, jobs)
	if _, err := source.Comments(context.Background(), issue); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("list did not fail for missing directory: %v", err)
	}
	if _, err := source.ReadComment(base.rows[0], issue); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("single read did not fail for missing directory: %v", err)
	}
	if _, err := os.Stat(jobs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created jobs: %v", err)
	}
}
