package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GitHub is one repository's issues as the engine's tracker. Nothing in the
// configuration chooses it yet; this is the side that reads.
type GitHub struct {
	// APIURL is the REST API's base: https://api.github.com when empty, or a
	// GitHub Enterprise Server's https://HOST/api/v3.
	APIURL string `json:"api_url,omitempty"`
	// Repository is owner/name; the issues are this repository's.
	Repository string `json:"repository"`
	KeyEnv     string `json:"key_env"`
	// IntakeLabel narrows the intake to the open issues carrying it. Empty
	// takes up every open issue.
	IntakeLabel string `json:"intake_label,omitempty"`
	// Labels names the label set on an issue at each turn of the work.
	Labels GitHubLabels `json:"labels"`
	Client *http.Client `json:"-"`
}

const (
	githubAPI = "https://api.github.com"
	// githubVersion is the REST API version the engine was written against,
	// the one the delivery harness asks for too.
	githubVersion = "2022-11-28"
	// GitHub refuses a request without a User-Agent. The module's own name
	// says what is asking.
	githubAgent = "ticket-runner"
	// A list page is a hundred records, each of which may carry a body of
	// tens of kilobytes, so a page may run longer than one record ever does.
	githubItemLimit = 4 << 20
	githubPageLimit = 32 << 20
	// githubPages bounds how many pages one list may take, so a server that
	// keeps pointing onward cannot keep the engine reading forever.
	githubPages = 1000
)

func (g GitHub) base() string {
	if g.APIURL == "" {
		return githubAPI
	}
	return strings.TrimRight(g.APIURL, "/")
}

// repository is owner/name, each part path-escaped, or an error for anything
// that is not one owner and one repository name.
func (g GitHub) repository() (string, error) {
	owner, name, found := strings.Cut(g.Repository, "/")
	for _, part := range []string{owner, name} {
		if !found || part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\?#%\r\n\t ") {
			return "", errors.New("the repository must be named as owner/name")
		}
	}
	return url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// issuePath is the API path of one issue, from its key: the issue's number.
func (g GitHub) issuePath(key string) (string, error) {
	repository, err := g.repository()
	if err != nil {
		return "", err
	}
	number, err := strconv.ParseInt(key, 10, 64)
	if err != nil || number <= 0 || strconv.FormatInt(number, 10) != key {
		return "", errors.New("a GitHub issue is named by its number")
	}
	return "/repos/" + repository + "/issues/" + key, nil
}

// ownURL says whether an API URL a record gives is the configured
// repository's, or an issue of it. Owner and name are matched without regard
// to case, as GitHub matches them.
func (g GitHub) ownURL(given, path string) bool {
	repository, err := g.repository()
	return err == nil && given != "" && strings.EqualFold(given, g.base()+"/repos/"+repository+path)
}

func (g GitHub) Identity() string {
	return fmt.Sprintf("Issue intake: %s\nRepository: %s", g.base(), strings.ToLower(g.Repository))
}

func (g GitHub) CredentialEnv() string { return g.KeyEnv }

// githubIssue is the part of an issue record the engine reads.
type githubIssue struct {
	Number        int64           `json:"number"`
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	CreatedAt     time.Time       `json:"created_at"`
	User          githubUser      `json:"user"`
	RepositoryURL string          `json:"repository_url"`
	PullRequest   json.RawMessage `json:"pull_request"`
	Labels        []githubLabel   `json:"labels"`
}

type githubUser struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// githubLabel is a label as an issue record lists it: an object with a name,
// or the name alone.
type githubLabel struct{ Name string }

func (l *githubLabel) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &l.Name); err == nil {
		return nil
	}
	var label struct {
		Name string `json:"name"`
	}
	err := json.Unmarshal(data, &label)
	l.Name = label.Name
	return err
}

// pullRequest says whether a record is a pull request's: GitHub lists every
// pull request among the issues, marked with this key.
func pullRequest(marker json.RawMessage) bool {
	return len(marker) > 0 && string(marker) != "null"
}

// Issues lists the repository's open issues, oldest first, and leaves the
// pull requests out: a delivery to the same repository opens one, and it is
// not a request.
func (g GitHub) Issues(ctx context.Context) ([]json.RawMessage, error) {
	repository, err := g.repository()
	if err != nil {
		return nil, err
	}
	query := url.Values{"state": {"open"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}}
	if g.IntakeLabel != "" {
		// The API reads a comma as between two labels.
		if strings.Contains(g.IntakeLabel, ",") {
			return nil, errors.New("the intake label must be one label, without a comma")
		}
		query.Set("labels", g.IntakeLabel)
	}
	rows, err := g.pages(ctx, "/repos/"+repository+"/issues?"+query.Encode())
	if err != nil {
		return nil, err
	}
	issues := []json.RawMessage{}
	seen := map[int64]bool{}
	for _, raw := range rows {
		// Only what places a record is read here; a record that cannot be read
		// for the rest is the intake's to leave aside, as with Backlog.
		var issue struct {
			Number        int64           `json:"number"`
			RepositoryURL string          `json:"repository_url"`
			PullRequest   json.RawMessage `json:"pull_request"`
		}
		if err := json.Unmarshal(raw, &issue); err != nil || issue.Number <= 0 {
			return nil, errors.New("tracker issue has no readable identity")
		}
		if !g.ownURL(issue.RepositoryURL, "") {
			return nil, errors.New("tracker returned an issue outside the configured repository")
		}
		if seen[issue.Number] {
			return nil, errors.New("tracker issue pagination repeated an issue; no partial list returned")
		}
		seen[issue.Number] = true
		if !pullRequest(issue.PullRequest) {
			issues = append(issues, raw)
		}
	}
	return issues, nil
}

func (g GitHub) ReadIssue(raw json.RawMessage) (Issue, error) {
	var issue githubIssue
	if err := json.Unmarshal(raw, &issue); err != nil || issue.Number <= 0 || pullRequest(issue.PullRequest) || !g.ownURL(issue.RepositoryURL, "") {
		return Issue{}, errors.New("the issue record could not be read for the configured repository")
	}
	return Issue{ID: issue.Number, Key: strconv.FormatInt(issue.Number, 10), Created: issue.CreatedAt,
		Creator: Account{ID: issue.User.ID, Login: issue.User.Login}, Raw: raw}, nil
}

func (g GitHub) Marked(issue Issue) bool {
	if g.IntakeLabel == "" {
		return true
	}
	var record githubIssue
	if json.Unmarshal(issue.Raw, &record) != nil {
		return false
	}
	for _, label := range record.Labels {
		if strings.EqualFold(label.Name, g.IntakeLabel) {
			return true
		}
	}
	return false
}

func (g GitHub) Request(ctx context.Context, key string) (string, error) {
	path, err := g.issuePath(key)
	if err != nil {
		return "", err
	}
	data, _, err := g.call(ctx, http.MethodGet, g.base()+path, nil, http.StatusOK, githubItemLimit)
	if err != nil {
		return "", err
	}
	var issue githubIssue
	if json.Unmarshal(data, &issue) == nil && pullRequest(issue.PullRequest) {
		return "", errors.New("the number is a pull request's, not an issue's")
	}
	return g.RequestText(data)
}

// RequestText renders an issue as Backlog's records are rendered, with the
// issue's number for its key.
func (g GitHub) RequestText(raw json.RawMessage) (string, error) {
	var issue githubIssue
	if err := json.Unmarshal(raw, &issue); err != nil {
		return "", errors.New("tracker response could not be read")
	}
	if issue.Number <= 0 {
		return "", errors.New("tracker response has no issue identity")
	}
	return "Original issue: " + strconv.FormatInt(issue.Number, 10) + "\nTitle: " + issue.Title + "\n\n" + issue.Body, nil
}

// Comments reads every comment on the issue. GitHub lists them in ascending
// id; a list that is not is not returned at all.
func (g GitHub) Comments(ctx context.Context, issue Issue) ([]json.RawMessage, error) {
	path, err := g.issuePath(issue.Key)
	if err != nil {
		return nil, err
	}
	rows, err := g.pages(ctx, path+"/comments?per_page=100")
	if err != nil {
		return nil, err
	}
	last := int64(0)
	for _, raw := range rows {
		var comment struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &comment); err != nil || comment.ID <= 0 {
			return nil, errors.New("tracker comment has no readable id")
		}
		if comment.ID <= last {
			return nil, errors.New("tracker comments did not come in ascending order; no incomplete list returned")
		}
		last = comment.ID
	}
	return rows, nil
}

// ReadComment places a comment by the issue its record names. A comment
// without a body or an author, as GitHub sends for a deleted account, is
// read as one without words or without an author.
func (g GitHub) ReadComment(raw json.RawMessage, issue Issue) (Comment, error) {
	var record struct {
		ID       int64      `json:"id"`
		Body     string     `json:"body"`
		User     githubUser `json:"user"`
		IssueURL string     `json:"issue_url"`
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return Comment{}, errors.New("the comment record could not be read")
	}
	return Comment{ID: record.ID, Body: record.Body, Author: Account{ID: record.User.ID, Login: record.User.Login},
		OnIssue: g.ownURL(record.IssueURL, "/issues/"+issue.Key)}, nil
}

func (g GitHub) CommentText(raw json.RawMessage) (int64, string, error) {
	var record struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(raw, &record); err != nil || record.ID <= 0 {
		return 0, "", errors.New("the comment record could not be read")
	}
	return record.ID, record.Body, nil
}

// Myself is the account the token belongs to, with the login GitHub assigns
// by.
func (g GitHub) Myself(ctx context.Context) (Account, error) {
	data, _, err := g.call(ctx, http.MethodGet, g.base()+"/user", nil, http.StatusOK, githubItemLimit)
	if err != nil {
		return Account{}, err
	}
	var me githubUser
	if err := json.Unmarshal(data, &me); err != nil || me.ID <= 0 || me.Login == "" {
		return Account{}, errors.New("tracker did not identify the credential's account")
	}
	return Account{ID: me.ID, Login: me.Login}, nil
}

// pages reads every page of a list, following the next page the answer
// names. GitHub names none when one page holds everything, and one page of
// exactly a hundred is then indistinguishable from one with more to come, so
// a full page without a next one is followed by the page after it. A page
// that cannot be read, or a next page on another host, fails the whole list.
func (g GitHub) pages(ctx context.Context, first string) ([]json.RawMessage, error) {
	address := g.base() + first
	rows := []json.RawMessage{}
	// A next page that is one already read would be read again and again,
	// each time out of the hourly allowance.
	read := map[string]bool{}
	for count := 0; ; count++ {
		if count == githubPages {
			return nil, fmt.Errorf("the list ran past %d pages; no partial list returned", githubPages)
		}
		if read[samePage(address)] {
			return nil, errors.New("tracker named as the next page one it had already given; no partial list returned")
		}
		read[samePage(address)] = true
		data, header, err := g.call(ctx, http.MethodGet, address, nil, http.StatusOK, githubPageLimit)
		if err != nil {
			return nil, err
		}
		var page []json.RawMessage
		if err := json.Unmarshal(data, &page); err != nil || page == nil || len(page) > 100 {
			return nil, errors.New("tracker page is not a bounded array")
		}
		rows = append(rows, page...)
		next, err := g.next(header.Get("Link"), address, len(page))
		if err != nil {
			return nil, err
		}
		if next == "" {
			return rows, nil
		}
		address = next
	}
}

var linkPart = regexp.MustCompile(`<([^>]*)>([^<]*)`)
var linkRelation = regexp.MustCompile(`(?i)\brel\s*=\s*"?([^";,]*)`)

// next is the page that follows, or empty when there is none.
func (g GitHub) next(link, current string, count int) (string, error) {
	for _, part := range linkPart.FindAllStringSubmatch(link, -1) {
		relation := linkRelation.FindStringSubmatch(part[2])
		if relation == nil || !strings.Contains(" "+strings.ToLower(relation[1])+" ", " next ") {
			continue
		}
		if !g.sameAPI(part[1]) {
			return "", errors.New("tracker named a next page outside its API; it was not followed and no partial list returned")
		}
		return part[1], nil
	}
	if count < 100 {
		return "", nil
	}
	address, err := url.Parse(current)
	if err != nil {
		return "", err
	}
	query := address.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	query.Set("page", strconv.Itoa(max(page, 1)+1))
	address.RawQuery = query.Encode()
	return address.String(), nil
}

// sameAPI says whether an address is under the configured API: the token goes
// to no other host, and to no path that a "." or ".." segment, written out or
// percent-encoded, could lead out of the API's own.
func (g GitHub) sameAPI(address string) bool {
	base, err := url.Parse(g.base())
	if err != nil {
		return false
	}
	given, err := url.Parse(address)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(given.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return given.Scheme == "https" && given.User == nil && strings.EqualFold(given.Host, base.Host) &&
		strings.HasPrefix(given.Path, strings.TrimRight(base.Path, "/")+"/")
}

// samePage is an address with its host's case and its query's order set
// aside, so that one page named twice is known for the same.
func samePage(address string) string {
	given, err := url.Parse(address)
	if err != nil {
		return address
	}
	return strings.ToLower(given.Host) + given.EscapedPath() + "?" + given.Query().Encode()
}

// githubError is an answer with a status other than the one expected.
type githubError struct {
	Status int
	Body   string
}

func (e *githubError) Error() string {
	return fmt.Sprintf("tracker returned HTTP %d: %s", e.Status, e.Body)
}

// call sends one request with the token in its header, never in its address,
// follows no redirect, and reads no more of the answer than limit. A moved
// repository answers 301, which is refused rather than followed: the token
// goes only where the configuration says, and the configuration is what to
// change.
func (g GitHub) call(ctx context.Context, method, address string, body any, expected, limit int) ([]byte, http.Header, error) {
	base, err := url.Parse(g.base())
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, nil, errors.New("tracker endpoint must be an HTTPS URL without credentials or query")
	}
	if !g.sameAPI(address) {
		return nil, nil, errors.New("tracker request outside the configured API")
	}
	token := os.Getenv(g.KeyEnv)
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, nil, errors.New("tracker credential is unavailable")
	}
	// GitHub asks a client it limited to send nothing until the time it gave;
	// one that goes on may be banned.
	shared := g.shared()
	if until := shared.closedUntil(githubNow()); !until.IsZero() {
		return nil, nil, fmt.Errorf("tracker asked to be sent nothing until %s; nothing was sent", until.UTC().Format(time.RFC3339))
	}
	read := method == http.MethodGet
	if !read {
		if err := shared.spaceChange(ctx); err != nil {
			return nil, nil, err
		}
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return nil, nil, errors.New("cannot construct tracker request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", githubVersion)
	request.Header.Set("User-Agent", githubAgent)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// A read asked again with the validator of the answer kept costs nothing
	// of the hourly allowance when GitHub answers that nothing changed.
	kept, conditional := shared.recall(address)
	conditional = conditional && read
	if conditional {
		request.Header.Set("If-None-Match", kept.etag)
	}
	client := http.Client{}
	if g.Client != nil {
		client = *g.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	redact := func(text string) string { return strings.ReplaceAll(text, token, "[credential]") }
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, errors.New(redact(err.Error()))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return nil, nil, errors.New(redact(err.Error()))
	}
	shared.learn(response.StatusCode, response.Header, data, githubNow())
	if conditional && response.StatusCode == http.StatusNotModified {
		// The answer kept, with its own headers: its next page is the one
		// it named.
		return kept.data, kept.header, nil
	}
	if len(data) > limit {
		return nil, nil, fmt.Errorf("tracker returned HTTP %d; response exceeds %d MiB; no truncated response returned", response.StatusCode, limit>>20)
	}
	if response.StatusCode != expected {
		if response.StatusCode >= 300 && response.StatusCode < 400 && response.StatusCode != http.StatusNotModified {
			// Nothing is sent where it points: the token goes only where the
			// configuration says.
			return nil, nil, fmt.Errorf("tracker returned HTTP %d, a redirect to %q, which is not followed: the repository or the issue may have been moved or renamed; check the configured repository",
				response.StatusCode, redact(response.Header.Get("Location")))
		}
		return nil, nil, &githubError{Status: response.StatusCode, Body: redact(string(data))}
	}
	if read {
		shared.keep(address, githubKept{etag: response.Header.Get("ETag"), data: data, header: response.Header.Clone()})
	}
	return data, response.Header, nil
}
