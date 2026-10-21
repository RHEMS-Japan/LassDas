package tracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Redmine is a Redmine server's REST API, as the operator configures it.
type Redmine struct {
	// BaseURL is the Redmine's own address, the one under which an issue's
	// page is /issues/<id>: https://redmine.example or
	// https://host.example/redmine.
	BaseURL string       `json:"base_url"`
	KeyEnv  string       `json:"key_env"`
	Client  *http.Client `json:"-"`
}

// RedmineProject is one Redmine project's issues as the engine's tracker.
// The ids are the operator's, from the engine's intake settings.
type RedmineProject struct {
	Client    Redmine
	ProjectID int64
	// Categories narrows the intake to issues carrying one of them; none
	// takes up every issue. A Redmine issue carries at most one.
	Categories []int64
	// Statuses names, by status id, where the issue moves at each turn of the
	// work; a turn without one leaves the status alone.
	Statuses map[string]int64
	// Since asks Redmine for the issues created at or after it only, so a
	// long-lived project is not listed whole on every poll. Zero asks for all.
	Since time.Time
}

var _ Tracker = RedmineProject{}

const (
	// One issue, or the account, is a small record. A list page holds a
	// hundred issues, and an issue read with its journals holds every note
	// on it, so either may run much longer.
	redmineItemLimit = 4 << 20
	redmineListLimit = 32 << 20
	// redminePages bounds how many pages one list may take.
	redminePages = 1000
)

func (r Redmine) base() string { return strings.TrimRight(r.BaseURL, "/") }

// redmineNumber reads an issue's key, its id, and refuses anything else.
func redmineNumber(key string) (int64, error) {
	number, err := strconv.ParseInt(key, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != key {
		return 0, errors.New("a Redmine issue is named by its id")
	}
	return number, nil
}

func (p RedmineProject) Identity() string {
	return fmt.Sprintf("Issue intake: %s\nRedmine project: %d", p.Client.base(), p.ProjectID)
}

func (p RedmineProject) CredentialEnv() string { return p.Client.KeyEnv }

// redmineIssue is the part of an issue record the engine reads.
type redmineIssue struct {
	ID          int64             `json:"id"`
	Subject     string            `json:"subject"`
	Description string            `json:"description"`
	CreatedOn   time.Time         `json:"created_on"`
	Project     redmineReference  `json:"project"`
	Author      redmineReference  `json:"author"`
	Category    *redmineReference `json:"category"`
	Status      redmineReference  `json:"status"`
	AssignedTo  *redmineReference `json:"assigned_to"`
}

// redmineReference is how Redmine names a project, an account, a status or
// a category inside another record.
type redmineReference struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Issues lists the project's open issues, oldest first. Subprojects' issues
// are left out: Redmine lists them with their parent's unless asked not to.
// The categories and the starting time narrow the list Redmine sends; the
// engine checks both again on its side. An issue whose author is not a
// person's account (see person) is left out too: everyone who files without
// an account is the same anonymous author, so anyone could answer or stop the
// request in its name.
func (p RedmineProject) Issues(ctx context.Context) ([]json.RawMessage, error) {
	if p.ProjectID <= 0 {
		return nil, errors.New("provide a positive project id for issue discovery")
	}
	query := url.Values{"project_id": {strconv.FormatInt(p.ProjectID, 10)}, "subproject_id": {"!*"}, "status_id": {"open"}}
	if !p.Since.IsZero() {
		// Redmine compares whole seconds; the engine compares the rest.
		query.Set("created_on", ">="+p.Since.UTC().Truncate(time.Second).Format(time.RFC3339))
	}
	if len(p.Categories) > 0 {
		ids := make([]string, len(p.Categories))
		for i, id := range p.Categories {
			ids[i] = strconv.FormatInt(id, 10)
		}
		query.Set("category_id", strings.Join(ids, "|"))
	}
	rows, err := p.Client.pages(ctx, query)
	if err != nil {
		return nil, err
	}
	issues := []json.RawMessage{}
	for _, raw := range rows {
		// Only what places a record is read here; a record that cannot be read
		// for the rest is the intake's to leave aside, as with the others.
		var issue struct {
			Project redmineReference `json:"project"`
			Author  redmineReference `json:"author"`
		}
		json.Unmarshal(raw, &issue) // pages read every id
		if issue.Project.ID != p.ProjectID {
			return nil, errors.New("tracker returned an issue outside the configured project")
		}
		person, err := p.Client.person(ctx, issue.Author.ID)
		if err != nil {
			return nil, err
		}
		if person {
			issues = append(issues, raw)
		}
	}
	return issues, nil
}

// pages reads every page of an issue list in ascending id, each page asking
// for the issues after the last one read. Paging by position would let an
// issue that leaves the list between two pages (closed, moved) shift another
// past the next page unseen. A page that cannot be read, or one that is not in
// ascending id after the last, fails the whole list.
func (r Redmine) pages(ctx context.Context, query url.Values) ([]json.RawMessage, error) {
	rows := []json.RawMessage{}
	last := int64(0)
	for count := 0; ; count++ {
		if count == redminePages {
			return nil, fmt.Errorf("the list ran past %d pages; no partial list returned", redminePages)
		}
		query.Set("sort", "id")
		query.Set("limit", "100")
		if last > 0 {
			query.Set("issue_id", ">="+strconv.FormatInt(last+1, 10))
		}
		data, err := r.call(ctx, http.MethodGet, "/issues.json", query, nil, redmineListLimit, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var page struct {
			Issues []json.RawMessage `json:"issues"`
		}
		if err := json.Unmarshal(data, &page); err != nil || page.Issues == nil || len(page.Issues) > 100 {
			return nil, errors.New("tracker issue page is not a bounded issue list")
		}
		for _, raw := range page.Issues {
			var issue struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(raw, &issue); err != nil || issue.ID <= 0 {
				return nil, errors.New("tracker issue has no readable identity")
			}
			if issue.ID <= last {
				return nil, errors.New("tracker did not list the issues in ascending id after the last one read; no partial list returned")
			}
			last = issue.ID
		}
		rows = append(rows, page.Issues...)
		if len(page.Issues) < 100 {
			return rows, nil
		}
	}
}

func (p RedmineProject) ReadIssue(raw json.RawMessage) (Issue, error) {
	var issue redmineIssue
	if err := json.Unmarshal(raw, &issue); err != nil || issue.ID <= 0 || issue.Project.ID != p.ProjectID {
		return Issue{}, errors.New("the issue record could not be read for the configured project")
	}
	return Issue{ID: issue.ID, Key: strconv.FormatInt(issue.ID, 10), Created: issue.CreatedOn,
		Creator: Account{ID: issue.Author.ID}, Raw: raw}, nil
}

func (p RedmineProject) Marked(issue Issue) bool {
	if len(p.Categories) == 0 {
		return true
	}
	var record redmineIssue
	if json.Unmarshal(issue.Raw, &record) != nil || record.Category == nil {
		return false
	}
	for _, id := range p.Categories {
		if record.Category.ID == id {
			return true
		}
	}
	return false
}

func (p RedmineProject) Request(ctx context.Context, key string) (string, error) {
	data, err := p.issue(ctx, key, false)
	if err != nil {
		return "", err
	}
	return p.RequestText(data)
}

// RequestText renders an issue as Backlog's records are rendered, with the
// issue's id for its key. The description is kept as Redmine stores it,
// line endings included.
func (p RedmineProject) RequestText(raw json.RawMessage) (string, error) {
	var issue redmineIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return "", errors.New("tracker response could not be read")
	}
	if issue.ID <= 0 {
		return "", errors.New("tracker response has no issue identity")
	}
	return "Original issue: " + strconv.FormatInt(issue.ID, 10) + "\nTitle: " + issue.Subject + "\n\n" + issue.Description, nil
}

// issue reads one issue's record, with its journals when asked, and returns
// it unwrapped from Redmine's {"issue": ...}. A record that is not the issue
// asked for is refused, and so is one in another project than the configured
// one: a request moved there stops instead of going on in that project.
func (p RedmineProject) issue(ctx context.Context, key string, journals bool) (json.RawMessage, error) {
	number, err := redmineNumber(key)
	if err != nil {
		return nil, err
	}
	var query url.Values
	limit := redmineItemLimit
	if journals {
		query, limit = url.Values{"include": {"journals"}}, redmineListLimit
	}
	data, err := p.Client.call(ctx, http.MethodGet, "/issues/"+key+".json", query, nil, limit, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Issue json.RawMessage `json:"issue"`
	}
	var issue struct {
		ID      int64            `json:"id"`
		Project redmineReference `json:"project"`
	}
	if json.Unmarshal(data, &answer) != nil || json.Unmarshal(answer.Issue, &issue) != nil || issue.ID != number {
		return nil, errors.New("tracker did not return the asked issue")
	}
	if p.ProjectID > 0 && issue.Project.ID != p.ProjectID {
		return nil, fmt.Errorf("issue %s is no longer in the configured project %d; nothing is read from it or written to it there", key, p.ProjectID)
	}
	return answer.Issue, nil
}

// journals reads every journal of the issue, in ascending id. A journal is a
// note, a change of the issue's fields, or both; one without words is listed
// too, as Backlog lists a change. Redmine leaves out the private notes the
// account may not see. Redmine lists journals by their time and then their
// id; the engine orders them by id, the order Redmine stored them in, so that
// a clock set back does not reorder an issue's history. A journal listed
// twice fails the whole list.
//
// Each record is the tracker's own, with two things added in front. The issue
// it was read from goes under "issue" where Redmine names none, so a record
// kept on its own still says where it came from. And "author_is_person" is
// false where the words' author (see redmineJournal.author) is not a person's account,
// so that nobody is taken to have written them.
func (p RedmineProject) journals(ctx context.Context, key string) ([]json.RawMessage, error) {
	data, err := p.issue(ctx, key, true)
	if err != nil {
		return nil, err
	}
	var issue struct {
		Journals []json.RawMessage `json:"journals"`
	}
	if err := json.Unmarshal(data, &issue); err != nil || issue.Journals == nil {
		return nil, errors.New("tracker returned the issue without its journals")
	}
	type entry struct {
		id  int64
		raw json.RawMessage
	}
	entries := make([]entry, 0, len(issue.Journals))
	seen := map[int64]bool{}
	for _, raw := range issue.Journals {
		var journal struct {
			redmineJournal
			Place json.RawMessage `json:"issue"`
		}
		trimmed := bytes.TrimSpace(raw)
		if err := json.Unmarshal(trimmed, &journal); err != nil || journal.ID <= 0 {
			return nil, errors.New("tracker journal has no readable id")
		}
		if seen[journal.ID] {
			return nil, errors.New("tracker listed a journal twice; no incomplete list returned")
		}
		seen[journal.ID] = true
		person, err := p.Client.person(ctx, journal.author())
		if err != nil {
			return nil, err
		}
		place := ""
		if journal.Place == nil {
			place = `"issue":{"id":` + key + `}`
		}
		if !person {
			place = strings.TrimPrefix(place+`,"author_is_person":false`, ",")
		}
		if place != "" {
			rest := bytes.TrimSpace(trimmed[1:])
			if rest[0] != '}' {
				place += ","
			}
			trimmed = append([]byte("{"+place), rest...)
		}
		entries = append(entries, entry{journal.ID, json.RawMessage(trimmed)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	rows := make([]json.RawMessage, len(entries))
	for i, entry := range entries {
		rows[i] = entry.raw
	}
	return rows, nil
}

func (p RedmineProject) Comments(ctx context.Context, issue Issue) ([]json.RawMessage, error) {
	return p.journals(ctx, issue.Key)
}

// redmineJournal is the part of a journal record the engine reads.
type redmineJournal struct {
	ID        int64             `json:"id"`
	Notes     *string           `json:"notes"`
	User      redmineReference  `json:"user"`
	UpdatedBy *redmineReference `json:"updated_by"`
	Issue     redmineReference  `json:"issue"`
	Private   bool              `json:"private_notes"`
	// Person is the engine's own mark: false when the author is not a
	// person's account.
	Person *bool `json:"author_is_person"`
}

// author is whose words the note holds: whoever wrote them last. Redmine 6.0
// names, under updated_by, the account that edited a note, which may not be
// the one that wrote it; an edited note is not its first author's answer or
// instruction. Redmine 5.x does not say in its API that a note was edited
// (5.1.12 gives neither updated_by nor updated_on), so there a note stays its
// first author's.
func (j redmineJournal) author() int64 {
	if j.UpdatedBy != nil && j.UpdatedBy.ID != j.User.ID {
		return j.UpdatedBy.ID
	}
	return j.User.ID
}

// ReadComment places a journal by the issue its record names, and gives as
// its author whoever wrote its words last, or nobody when that is not a
// person's account.
func (p RedmineProject) ReadComment(raw json.RawMessage, issue Issue) (Comment, error) {
	var record redmineJournal
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return Comment{}, errors.New("the comment record could not be read")
	}
	body := ""
	if record.Notes != nil {
		body = *record.Notes
	}
	author := record.author()
	if record.Person != nil && !*record.Person {
		author = 0
	}
	return Comment{ID: record.ID, Body: body, Author: Account{ID: author},
		OnIssue: issue.ID > 0 && record.Issue.ID == issue.ID}, nil
}

func (p RedmineProject) CommentText(raw json.RawMessage) (int64, string, error) {
	var record struct {
		ID    int64   `json:"id"`
		Notes *string `json:"notes"`
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return 0, "", errors.New("the comment record could not be read")
	}
	if record.Notes == nil {
		return record.ID, "", nil
	}
	return record.ID, *record.Notes, nil
}

// Myself is the account the key belongs to.
func (p RedmineProject) Myself(ctx context.Context) (Account, error) {
	return p.Client.myself(ctx)
}

// redmineKnown keeps, for the life of the process, what Redmine does not
// change while the engine runs: the account a key belongs to, and whether an
// account id is a person's account. Every caller (the intake, the queue, the
// watcher of each request, each role's scope) asks the same Redmine with the
// same key, so what one learns, all keep. Answers are kept per address and
// key; a failed question keeps nothing.
var redmineKnown = struct {
	sync.Mutex
	me      map[string]Account
	persons map[string]bool
}{me: map[string]Account{}, persons: map[string]bool{}}

// known is the place the process keeps answers for this address and key.
func (r Redmine) known() string {
	sum := sha256.Sum256([]byte(os.Getenv(r.KeyEnv)))
	return r.base() + "\x00" + hex.EncodeToString(sum[:])
}

func (r Redmine) myself(ctx context.Context) (Account, error) {
	place := r.known()
	redmineKnown.Lock()
	me, found := redmineKnown.me[place]
	redmineKnown.Unlock()
	if found {
		return me, nil
	}
	data, err := r.call(ctx, http.MethodGet, "/users/current.json", nil, nil, redmineItemLimit, http.StatusOK)
	if err != nil {
		return Account{}, err
	}
	var answer struct {
		User struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &answer); err != nil || answer.User.ID <= 0 {
		return Account{}, errors.New("tracker did not identify the credential's account")
	}
	me = Account{ID: answer.User.ID, Login: answer.User.Login}
	redmineKnown.Lock()
	redmineKnown.me[place] = me
	redmineKnown.Unlock()
	return me, nil
}

// person says whether an account id is a person's account: one the key can
// look up (GET /users/<id>.json) and that has a login. Redmine's anonymous
// user, the author of everything filed or written without an account, has
// none: Redmine 5.1 and 6.0 answer 404 for it to a key that is not an
// administrator's, and give an administrator's an empty login. Its id and its
// name (which is translated) differ from one Redmine to the next, so neither
// is compared. An account the key may not look up counts the same: the
// engine does not act on words it cannot place.
func (r Redmine) person(ctx context.Context, id int64) (bool, error) {
	if id <= 0 {
		return false, nil
	}
	place := r.known() + "\x00" + strconv.FormatInt(id, 10)
	redmineKnown.Lock()
	person, found := redmineKnown.persons[place]
	redmineKnown.Unlock()
	if found {
		return person, nil
	}
	data, err := r.call(ctx, http.MethodGet, "/users/"+strconv.FormatInt(id, 10)+".json", nil, nil, redmineItemLimit, http.StatusOK)
	var refusal *trackerError
	switch {
	case errors.As(err, &refusal) && refusal.Status == http.StatusNotFound:
		person = false
	case err != nil:
		return false, fmt.Errorf("the author %d of a record could not be looked up: %w", id, err)
	default:
		var answer struct {
			User struct {
				ID    int64  `json:"id"`
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(data, &answer); err != nil || answer.User.ID != id {
			return false, fmt.Errorf("the author %d of a record could not be looked up: Redmine did not answer with that account", id)
		}
		person = answer.User.Login != ""
	}
	redmineKnown.Lock()
	redmineKnown.persons[place] = person
	redmineKnown.Unlock()
	return person, nil
}

// call sends one request with the key in its header, never in its address,
// follows no redirect, and reads no more of the answer than limit. Any of the
// expected statuses is success: Redmine answers a change with 204, and has
// answered some with 200.
func (r Redmine) call(ctx context.Context, method, path string, query url.Values, body any, limit int, expected ...int) ([]byte, error) {
	base, err := url.Parse(r.base())
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return nil, errors.New("tracker endpoint must be an HTTPS URL without credentials or query")
	}
	key := os.Getenv(r.KeyEnv)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("tracker credential is unavailable")
	}
	address := r.base() + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return nil, errors.New("cannot construct tracker request")
	}
	request.Header.Set("X-Redmine-API-Key", key)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{}
	if r.Client != nil {
		client = *r.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	redact := func(text string) string { return strings.ReplaceAll(text, key, "[credential]") }
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New(redact(err.Error()))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, errors.New(redact(err.Error()))
	}
	if len(data) > limit {
		return nil, fmt.Errorf("tracker returned HTTP %d; response exceeds %d MiB; no truncated response returned", response.StatusCode, limit>>20)
	}
	for _, status := range expected {
		if response.StatusCode == status {
			return data, nil
		}
	}
	if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != http.StatusNotModified {
		// The key goes only where the configuration says. The address is
		// quoted in short, the key taken out before it is cut.
		return nil, fmt.Errorf("tracker returned HTTP %d, a redirect to %q, which is not followed: check redmine.base_url, which must be the address Redmine serves its pages under",
			response.StatusCode, clip(redact(response.Header.Get("Location")), 200))
	}
	return nil, &trackerError{Status: response.StatusCode, Body: redact(string(data))}
}
