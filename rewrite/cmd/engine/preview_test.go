package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ticket-runner/internal/chain"
)

// The address the stand-in preview role puts the change up at, and the
// credential it is handed for that. Both are made up.
const (
	previewAddress = "https://sample-51.preview.example.invalid/"
	previewToken   = "synthetic-preview-token-5f3a9c"
	previewUntil   = "この画面は 10/17 18:00 まで開けます"
)

// previewUpScript is the stand-in for the operator's script: it keeps a copy
// of the request's preview record as it found it at launch, counts its
// launches, and writes the address, a line for the requester and, to show it
// is kept out of every record, the credential it was handed.
const previewUpScript = `cat > /dev/null
cp ../preview.json preview-at-launch.json 2>/dev/null
echo up >> preview-launches.txt
mkdir -p "$(dirname "$TASK_PREVIEW")"
printf '%s\n%s\n%s\n' "https://sample-51.preview.example.invalid/" "この画面は 10/17 18:00 まで開けます" "token $PREVIEW_K8S_TOKEN" > "$TASK_PREVIEW"
echo "put up with $PREVIEW_K8S_TOKEN"`

// previewStagesConfig is an ordered run that reads the change before it is
// delivered and asks the requester about it, with the operator's two preview
// roles. Every role is a plain command standing in for the real one.
func previewStagesConfig(t *testing.T) config {
	t.Helper()
	cfg := watchConfiguration(t)
	t.Setenv("PREVIEW_TEST_TOKEN", previewToken)
	cfg.Router.Mode = "stages"
	cfg.ModelSelection = &selectionConfig{Fixed: "maker/configured"}
	cfg.Intake.QuestionRole = "ask_requester"
	sh := func(script string) []string { return []string{"/bin/sh", "-c", script} }
	cfg.Roles = []chain.Role{
		{Name: "elicit", Purpose: "Settle the requirements.", Processes: []chain.Process{{Name: "requirements", ModelEnv: "MODEL",
			Command: sh(`cat > /dev/null; env > elicit-env.txt; echo 'Settled requirements.'`)}}},
		{Name: "confirm_change", Purpose: "Read the change before it is delivered.", Processes: []chain.Process{{Name: "reader", ModelEnv: "MODEL",
			Command: sh(`cat > /dev/null; echo 'The list screen gains a count line.'`)}}},
		{Name: "deliver", Purpose: "Deliver the change.", Processes: []chain.Process{{Name: "delivery", Command: sh(`cat > /dev/null; echo delivered`)}}},
		{Name: "ask_requester", Purpose: "Ask the requester.", Processes: []chain.Process{{Name: "questioner", TrackerAccess: "comment",
			Command: sh(`cat > question-prompt.txt; echo 'Asked in ordinary prose.'`)}}},
		{Name: "preview_up", Purpose: "Put the change up for the requester to see.", Processes: []chain.Process{{Name: "up", TimeoutMinutes: 10,
			Secrets: map[string]string{"PREVIEW_K8S_TOKEN": "PREVIEW_TEST_TOKEN"}, Command: sh(previewUpScript)}}},
		{Name: "preview_down", Purpose: "Take the page down.", Processes: []chain.Process{{Name: "down", TimeoutMinutes: 3,
			Secrets: map[string]string{"PREVIEW_K8S_TOKEN": "PREVIEW_TEST_TOKEN"}, Command: sh(`cat > /dev/null; echo down >> preview-downs.txt`)}}},
	}
	cfg.Workflow = &chain.Workflow{Stages: []chain.Stage{
		{Name: "elicit", Kind: chain.ModelStage},
		{Name: "confirm_change", Kind: chain.ModelStage, Confirm: true},
		{Name: "deliver", Kind: chain.CommandStage, OnFailure: "elicit"},
	}, Preview: &chain.Preview{Up: "preview_up", Down: "preview_down"}}
	return cfg
}

// askAfterReading stands in for the decision service of previewStagesConfig:
// the change is read after the requirements, and the requester is asked
// about it once it has been read.
func askAfterReading(r *http.Request) (*http.Response, error) {
	var input struct{ State chain.State }
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return nil, err
	}
	choice := "confirm_change"
	if slices.ContainsFunc(input.State.History, func(result chain.Result) bool { return result.Role == "confirm_change" }) {
		choice = "ask_requester"
	}
	return selectionReply(r, 200, map[string]any{"answers": map[string]any{"next": map[string]string{"choice": choice}}}), nil
}

func readPreviewFile(t *testing.T, path string) previewRecord {
	t.Helper()
	var record previewRecord
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no preview record at %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatalf("unreadable preview record: %s", raw)
	}
	return record
}

// The decision after the change was read chooses to ask the requester. The
// page goes up first, its address is posted once in the runtime's own words
// before the question is launched, and the question is told the address and
// that it was posted. The record of it is the runtime's, with nothing in its
// error field, so the choice of a model never reads it as a failure, and the
// credential the preview role was handed appears nowhere.
func TestThePagesAddressIsPostedOnceBeforeTheConfirmationQuestion(t *testing.T) {
	cfg := previewStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, askAfterReading)
	root, directory := noticeJob(t, chain.State{})
	if err := os.Remove(filepath.Join(directory, "run", "history.json")); err != nil {
		t.Fatal(err)
	}
	finish := startStopQueue(t, cfg, root, 20*time.Millisecond, io.Discard)
	waitFor(t, func() bool {
		state, err := savedHistory(directory)
		_, recorded := os.Stat(filepath.Join(directory, "question.json"))
		return err == nil && state.Waiting && recorded == nil
	})
	finish()
	workspace := filepath.Join(directory, "workspace")
	want := "確認用の画面: " + previewAddress + "\n" + previewUntil + "\ntoken [credential]"
	if posted := fixture.withPrefix("確認用の画面"); len(posted) != 1 || posted[0] != want {
		t.Fatalf("the address was not posted once in the runtime's words:\n%q", posted)
	}
	if launches := string(readFile(t, filepath.Join(workspace, "preview-launches.txt"))); launches != "up\n" {
		t.Fatalf("the preview role ran %q", launches)
	}
	// What the role wrote, its credential included, is not kept in its home
	// once the engine has read it.
	if _, err := os.Lstat(filepath.Join(directory, "homes", "4-0", "task-preview.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the address file is left in the role's home: %v", err)
	}
	if record := readPreviewFile(t, filepath.Join(workspace, "preview-at-launch.json")); record.State != "starting" || record.StartedAt.IsZero() {
		t.Fatalf("the request did not record that a page was going up before the role ran: %+v", record)
	}
	if record := readPreviewFile(t, filepath.Join(directory, previewFile)); record.State != "up" || record.URL != previewAddress {
		t.Fatalf("the request's preview record does not say the page is up: %+v", record)
	}
	state := loadJobState(t, directory)
	var notes []chain.Result
	var question chain.Result
	for _, result := range state.History {
		if result.Role == "router" && strings.Contains(result.Output, "確認用の画面") {
			notes = append(notes, result)
		}
		if result.Role == "ask_requester" && result.Speaker == "questioner" {
			question = result
		}
	}
	if len(notes) != 1 || notes[0].Speaker != "runtime" || notes[0].Error != "" || !strings.Contains(notes[0].Output, previewAddress) {
		t.Fatalf("the runtime did not keep one record of the page without an error: %+v", notes)
	}
	for _, failure := range selectionInput(state).History {
		if strings.Contains(failure.Error, "確認用の画面") || strings.Contains(failure.Output, "確認用の画面") {
			t.Fatalf("the page reached the model choice as a failure: %+v", failure)
		}
	}
	var postedAt time.Time
	for _, record := range readNotices(t, directory).Notices {
		if record.Kind == previewNotice && record.PostedAt != nil {
			postedAt = *record.PostedAt
		}
	}
	if postedAt.IsZero() || question.StartedAt.IsZero() || !postedAt.Before(question.StartedAt) {
		t.Fatalf("the address was not confirmed posted before the question was launched: posted %v, question %v", postedAt, question.StartedAt)
	}
	prompt := string(readFile(t, filepath.Join(workspace, "question-prompt.txt")))
	if !strings.Contains(prompt, chainPreviewPosted(previewAddress)) {
		t.Fatalf("the question was not told the address and that it was posted:\n%s", prompt)
	}
	// The credential went to the preview role and nowhere else.
	for _, path := range []string{filepath.Join(directory, "run", "history.json"), filepath.Join(directory, "notices.json"),
		filepath.Join(directory, previewFile), filepath.Join(workspace, "question-prompt.txt")} {
		if bytes.Contains(readFile(t, path), []byte(previewToken)) {
			t.Fatalf("the preview credential is in %s", path)
		}
	}
	for _, posted := range fixture.all() {
		if strings.Contains(posted, previewToken) {
			t.Fatalf("the preview credential was posted: %q", posted)
		}
	}
	env := string(readFile(t, filepath.Join(workspace, "elicit-env.txt")))
	if strings.Contains(env, previewToken) || strings.Contains(env, previewEnv+"=") {
		t.Fatalf("another role was handed the preview role's credential or file:\n%s", env)
	}
}

// chainPreviewPosted is what the question is told when the page is up and its
// address was posted.
func chainPreviewPosted(address string) string {
	return strings.TrimSuffix(previewPostedInstruction(address), "\n")
}

// previewJob lays down one accepted request for the previewer alone, with the
// queue's record of notice kinds as an engine with this configuration leaves
// it, and returns a previewer bound to it as the run that asks would be.
func previewJob(t *testing.T, cfg config) (string, previewer) {
	t.Helper()
	root, directory := noticeJob(t, chain.State{})
	queueRanSince(t, root, cfg, time.Now().Add(-time.Hour))
	if err := os.MkdirAll(filepath.Join(directory, "workspace"), 0700); err != nil {
		t.Fatal(err)
	}
	bound, err := bindRequestConfig(cfg, directory, "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]chain.Role{}
	for _, role := range bound.Roles {
		roles[role.Name] = role
	}
	return directory, previewer{cfg: bound, executor: chain.Processes{Roles: roles}, job: directory, key: "EXAMPLE-51", observe: func(string) {}}
}

// withPreviewScript gives the preview role another script, and a time limit
// when one is given.
func withPreviewScript(p previewer, script string, limit time.Duration) previewer {
	processes := p.executor.(chain.Processes)
	roles := map[string]chain.Role{}
	for name, role := range processes.Roles {
		roles[name] = role
	}
	role := roles["preview_up"]
	role.Processes = slices.Clone(role.Processes)
	role.Processes[0].Command = []string{"/bin/sh", "-c", script}
	role.Processes[0].Timeout = limit
	roles["preview_up"] = role
	processes.Roles = roles
	p.executor = processes
	return p
}

func previewFileOf(p previewer) string {
	return p.executor.(chain.Processes).Roles["preview_up"].Processes[0].Env[previewEnv]
}

// A page that did not come up leaves the question as it was before the
// setting: the question is told to say in one line that the page could not
// be prepared and to describe the change in words. Nothing is posted, and
// the request's record keeps that the page may have been partly made, so it
// is still taken down later. A non-zero exit, the time limit, a first line
// that is not an https address without spaces, nothing written, the address
// a previous launch left, and a file that is not an ordinary one all count
// as a page that did not come up.
func TestAPageThatDidNotComeUpLeavesTheQuestionInWords(t *testing.T) {
	write := func(first string) string {
		return `cat > /dev/null; mkdir -p "$(dirname "$TASK_PREVIEW")"; printf '%s\n' '` + first + `' > "$TASK_PREVIEW"`
	}
	for _, mode := range []struct {
		name, script string
		limit        time.Duration
	}{
		{"exit-1", write(previewAddress) + `; echo "the cluster refused the deployment for $PREVIEW_K8S_TOKEN" >&2; exit 1`, 0},
		{"time-limit", `cat > /dev/null; exec sleep 5`, 300 * time.Millisecond},
		{"not-https", write("http://sample-51.preview.example.invalid/"), 0},
		{"space-in-address", write(previewAddress + " (ready)"), 0},
		{"empty-first-line", write(""), 0},
		{"nothing-written", `cat > /dev/null; exit 0`, 0},
		{"left-by-the-previous-launch", `cat > /dev/null; exit 0`, 0},
		{"link", `cat > /dev/null; mkdir -p "$(dirname "$TASK_PREVIEW")"; printf '%s\n' '` + previewAddress + `' > "$TASK_PREVIEW.real"; ln -s "$TASK_PREVIEW.real" "$TASK_PREVIEW"`, 0},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := previewStagesConfig(t)
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			directory, p := previewJob(t, cfg)
			p = withPreviewScript(p, mode.script, mode.limit)
			if mode.name == "left-by-the-previous-launch" {
				file := previewFileOf(p)
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(previewAddress+"\n"+previewUntil+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			state := chain.State{Request: noticeRequest, History: []chain.Result{{Role: "confirm_change", Speaker: "reader", Output: "Read."}}}
			note, instruction := p.up(context.Background(), "preview_up", state)
			if instruction != previewFailedInstruction {
				t.Fatalf("the question was not told the page could not be prepared: %q (note %q)", instruction, note)
			}
			if !strings.HasPrefix(note, "確認用の画面を用意できなかった") || strings.Contains(note, previewAddress) && mode.name != "space-in-address" {
				t.Fatalf("the runtime's note does not say the page did not come up: %q", note)
			}
			if posted := fixture.all(); len(posted) != 0 {
				t.Fatalf("a page that did not come up was posted: %q", posted)
			}
			if _, err := os.Lstat(previewFileOf(p)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the address file is left in the role's home after it was read: %v", err)
			}
			if record := readPreviewFile(t, filepath.Join(directory, previewFile)); record.State != "failed" || record.Reason == "" || record.URL != "" {
				t.Fatalf("the request's record does not keep a page that may be partly made: %+v", record)
			}
			if mode.name == "exit-1" {
				// What the role said on its way out is kept, with the
				// credential it was handed replaced.
				if !strings.Contains(note, "the cluster refused the deployment for [credential]") {
					t.Fatalf("the reason the role gave is not in the note: %q", note)
				}
				if strings.Contains(note, previewToken) || bytes.Contains(readFile(t, filepath.Join(directory, previewFile)), []byte(previewToken)) {
					t.Fatal("the preview credential is in the record of a page that did not come up")
				}
			}
		})
	}
}

// The address file is read as text for a person: no more than 16 KiB, in
// UTF-8, and without control characters other than the line breaks; a tab
// is let through in the words for the requester, not in the address, which
// also holds no invisible formatting character, and the words hold none that
// turns the direction of text. A file that is not is a page that did not come
// up: the question is told so and nothing is posted. The right forms come
// up: Windows line ends, a tab in the words, Japanese and emoji, and what is
// posted holds no carriage return.
func TestAnAddressFileThatIsNotPlainTextIsAPageThatDidNotComeUp(t *testing.T) {
	first := previewAddress + "\n"
	for _, form := range []struct {
		name    string
		content []byte
	}{
		{"too-large", []byte(first + strings.Repeat("a", previewBytes))},
		{"not-utf8", []byte(first + "\xff\xfe\n")},
		{"nul", []byte(first + "before\x00after\n")},
		{"esc", []byte(first + "\x1b[31mred\x1b[0m\n")},
		{"soh", []byte(first + "\x01\n")},
		{"del", []byte(first + "a\x7fb\n")},
		{"control-on-the-first-line", []byte(previewAddress + "\x1b\n")},
		{"tab-on-the-first-line", []byte(previewAddress + "\t\n")},
		{"zero-width-space-on-the-first-line", []byte(previewAddress + "\u200b\n")},
		{"right-to-left-override-on-the-first-line", []byte(previewAddress + "\u202e\n")},
		{"right-to-left-override-in-the-words", []byte(first + "\u202e" + "reversed\n")},
		{"isolate-in-the-words", []byte(first + "\u2066" + "isolated\u2069\n")},
		{"right-form", []byte(previewAddress + "\r\n" + previewUntil + "\r\n")},
		{"tab-in-the-words", []byte(first + "変更:\tボタンの色\n")},
		{"japanese-and-emoji-in-the-words", []byte(first + "確認してください \U0001F468\u200d\U0001F469\u200d\U0001F467 \u2705\n")},
	} {
		t.Run(form.name, func(t *testing.T) {
			cfg := previewStagesConfig(t)
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			directory, p := previewJob(t, cfg)
			source := filepath.Join(t.TempDir(), "address")
			if err := os.WriteFile(source, form.content, 0600); err != nil {
				t.Fatal(err)
			}
			p = withPreviewScript(p, `cat > /dev/null; mkdir -p "$(dirname "$TASK_PREVIEW")"; cp '`+source+`' "$TASK_PREVIEW"`, 0)
			state := chain.State{Request: noticeRequest, History: []chain.Result{{Role: "confirm_change", Speaker: "reader", Output: "Read."}}}
			note, instruction := p.up(context.Background(), "preview_up", state)
			record := readPreviewFile(t, filepath.Join(directory, previewFile))
			if wants, up := map[string]string{"right-form": previewUntil, "tab-in-the-words": "変更:\tボタンの色",
				"japanese-and-emoji-in-the-words": "確認してください \U0001F468\u200d\U0001F469\u200d\U0001F467 \u2705"}[form.name]; up {
				want := "確認用の画面: " + previewAddress + "\n" + wants
				if instruction != previewPostedInstruction(previewAddress) || record.State != "up" || fixture.count(want) != 1 {
					t.Fatalf("a file in the right form did not come up as %q: %q %+v %q", want, instruction, record, fixture.all())
				}
				return
			}
			if instruction != previewFailedInstruction || !strings.HasPrefix(note, "確認用の画面を用意できなかった") || record.State != "failed" {
				t.Fatalf("the file was taken for a page that came up: %q %q %+v", instruction, note, record)
			}
			if posted := fixture.all(); len(posted) != 0 {
				t.Fatalf("a file that is not plain text was posted: %q", posted)
			}
		})
	}
}

// The address is posted once each time the page is put up for a question.
// The runtime may be restarted after posting and before its record was
// saved, which asks the decision again and puts the page up again at the same
// point of the record: that is not posted a second time, and neither is a
// post the runtime recorded and was stopped before submitting. A page put up
// again later, for a question about a corrected change, is posted again.
func TestThePagesAddressIsPostedOncePerPutUp(t *testing.T) {
	cfg := previewStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, p := previewJob(t, cfg)
	state := chain.State{Request: noticeRequest, History: []chain.Result{{Role: "confirm_change", Speaker: "reader", Output: "Read."}}}
	want := "確認用の画面: " + previewAddress + "\n" + previewUntil + "\ntoken [credential]"
	// A post recorded and then cut off before it was submitted.
	n := requestNotices(cfg, sourceIssue{ID: 51, Key: "EXAMPLE-51"}, directory)
	if err := n.save(noticeLog{Notices: []noticeRecord{{Kind: previewNotice, Text: want, Event: previewPoint(state.History), WrittenAt: time.Now().UTC()}}}); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, instruction := p.up(context.Background(), "preview_up", state); instruction != previewPostedInstruction(previewAddress) {
			t.Fatalf("attempt %d: the question was not told the address was posted: %q", attempt, instruction)
		}
	}
	if posted := fixture.count(want); posted != 1 {
		t.Fatalf("one put-up was posted %d times", posted)
	}
	// The requester asked for a correction; the corrected change is asked
	// about again later in the record.
	state.History = append(state.History, chain.Result{Role: "ask_requester", Speaker: "requester", Output: "ここを直して"},
		chain.Result{Role: "confirm_change", Speaker: "reader", Output: "Read again."})
	if _, instruction := p.up(context.Background(), "preview_up", state); instruction != previewPostedInstruction(previewAddress) {
		t.Fatalf("the later put-up was not posted: %q", instruction)
	}
	if posted := fixture.count(want); posted != 2 {
		t.Fatalf("two put-ups were posted %d times", posted)
	}
}

// A page that came up but whose address could not be confirmed posted still
// reaches the requester through the question: it is told the address and to
// give it, not that it was posted. The page is up as far as the request's
// record goes, and the runtime's note says the comment was not confirmed.
func TestAnAddressThatCouldNotBePostedIsGivenByTheQuestion(t *testing.T) {
	cfg := previewStagesConfig(t)
	fixture := &noticeTracker{postFail: 100}
	fixture.install(t, alwaysChoose("done"))
	directory, p := previewJob(t, cfg)
	state := chain.State{Request: noticeRequest, History: []chain.Result{{Role: "confirm_change", Speaker: "reader", Output: "Read."}}}
	note, instruction := p.up(context.Background(), "preview_up", state)
	if instruction != previewUnconfirmedInstruction(previewAddress) || strings.Contains(instruction, "has posted") {
		t.Fatalf("the question was not told to give the address itself: %q", instruction)
	}
	if !strings.Contains(note, previewAddress) || !strings.Contains(note, "投稿を確かめられなかった") {
		t.Fatalf("the runtime's note does not say the comment was not confirmed: %q", note)
	}
	if record := readPreviewFile(t, filepath.Join(directory, previewFile)); record.State != "up" || record.URL != previewAddress {
		t.Fatalf("a page that came up is not recorded up: %+v", record)
	}
	if posted := fixture.all(); len(posted) != 0 || fixture.attempts() == 0 {
		t.Fatalf("the address was not tried, or a refused post was kept: %q after %d attempts", posted, fixture.attempts())
	}
}

// previewRouter asks the requester after the change was read, as the
// decision service does in these tests. With unavailableOnce, the first
// decision after the runtime was lost fails once before it asks.
type previewRouter struct {
	store           *crashBeforeQuestion
	unavailableOnce bool
	failed          bool
}

func (r *previewRouter) Next(context.Context, chain.State) (chain.Assignment, error) {
	if r.unavailableOnce && r.store.crashed && !r.failed {
		r.failed = true
		return chain.Assignment{}, errors.New("the decision service is unavailable once")
	}
	return chain.Assignment{Role: "ask_requester"}, nil
}

// crashBeforeQuestion is the request's store, which loses the runtime the
// first time the question is about to be recorded as pending: right after
// the page went up and its address was posted, before the record of it was
// saved.
type crashBeforeQuestion struct {
	state   chain.State
	crashed bool
	stop    context.CancelFunc
}

func (s *crashBeforeQuestion) Load() (chain.State, error) { return s.state, nil }

func (s *crashBeforeQuestion) Save(state chain.State) error {
	if !s.crashed && state.Pending != nil && state.Pending.Role == "ask_requester" {
		s.crashed = true
		s.stop()
		return errors.New("the runtime is gone")
	}
	s.state = state
	return nil
}

// The runtime is lost right after the page went up and its address was
// posted, before the record of either was saved. The next start finds the
// question not yet chosen, decides again and puts the page up again: the
// address is not posted a second time, and the question is told it was. That
// holds too when the first decision after the restart was unavailable and
// left the runtime's record of that failure before the question was chosen.
func TestARestartAfterTheAddressWasPostedDoesNotPostItAgain(t *testing.T) {
	for _, decision := range []string{"available", "unavailable-once"} {
		t.Run(decision, func(t *testing.T) { testRestartAfterTheAddressWasPosted(t, decision == "unavailable-once") })
	}
}

func testRestartAfterTheAddressWasPosted(t *testing.T, unavailableOnce bool) {
	cfg := previewStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	directory, p := previewJob(t, cfg)
	workflow := *cfg.Workflow
	workflow.Question = "ask_requester"
	store := &crashBeforeQuestion{state: chain.State{Request: noticeRequest, Workflow: &workflow, Step: "confirm_change",
		History: []chain.Result{{Role: "elicit", Speaker: "requirements", Output: "Settled."}, {Role: "confirm_change", Speaker: "reader", Output: "The list screen gains a count line."}}}}
	var asked []string
	router := &previewRouter{store: store, unavailableOnce: unavailableOnce}
	engine := chain.Chain{Router: router, Store: store, WaitAfter: "ask_requester", RetryDelay: time.Millisecond, PreviewUp: p.up,
		Executor: questionStandIn(func(a chain.Assignment) { asked = append(asked, a.Instruction) })}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	store.stop = cancel
	if err := engine.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the runtime was not lost before the question: %v", err)
	}
	if posted := fixture.withPrefix("確認用の画面"); len(posted) != 1 || len(asked) != 0 {
		t.Fatalf("before the loss the address was posted %d times and the question asked %d times", len(posted), len(asked))
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := engine.Run(ctx); !errors.Is(err, chain.ErrWaiting) {
		t.Fatalf("the restarted run did not ask and wait: %v", err)
	}
	if launches := string(readFile(t, filepath.Join(directory, "workspace", "preview-launches.txt"))); launches != "up\nup\n" {
		t.Fatalf("the page was not put up again after the restart: %q", launches)
	}
	if posted := fixture.withPrefix("確認用の画面"); len(posted) != 1 {
		t.Fatalf("the address was posted %d times across the restart", len(posted))
	}
	if len(asked) != 1 || !strings.Contains(asked[0], chainPreviewPosted(previewAddress)) {
		t.Fatalf("the question was not told the address was posted: %q", asked)
	}
	if router.failed != unavailableOnce {
		t.Fatalf("the decision after the restart failed=%v, want %v", router.failed, unavailableOnce)
	}
}

// questionStandIn is the question role, which only notes what it was told.
type questionStandIn func(chain.Assignment)

func (q questionStandIn) Execute(_ context.Context, a chain.Assignment, _ chain.State) []chain.Result {
	q(a)
	return []chain.Result{{Role: a.Role, Speaker: "questioner", Output: "Asked.", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC()}}
}

// The run posts the address while the watcher of the same request may be
// settling an earlier notice. The two never submit the same notice twice.
func TestThePagesAddressAndTheWatchersRetryDoNotPostTwice(t *testing.T) {
	cfg := previewStagesConfig(t)
	fixture := &noticeTracker{}
	fixture.install(t, alwaysChoose("done"))
	slow := http.DefaultTransport
	useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			time.Sleep(150 * time.Millisecond)
		}
		return slow.RoundTrip(r)
	})
	directory, p := previewJob(t, cfg)
	n := requestNotices(cfg, sourceIssue{ID: 51, Key: "EXAMPLE-51"}, directory)
	state := chain.State{Request: noticeRequest, History: []chain.Result{{Role: "confirm_change", Speaker: "reader", Output: "Read."}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var watching sync.WaitGroup
	watching.Add(1)
	go func() {
		defer watching.Done()
		for ctx.Err() == nil {
			_ = n.flush(ctx)
			time.Sleep(5 * time.Millisecond)
		}
	}()
	p.up(context.Background(), "preview_up", state)
	cancel()
	watching.Wait()
	if posted := fixture.withPrefix("確認用の画面"); len(posted) != 1 {
		t.Fatalf("the address was posted %d times", len(posted))
	}
}

// Each request's notices are said one at a time and no request waits for
// another's: two notices of one request take the time of two posts, also on
// an installation without the preview setting, and notices of two requests
// take the time of one.
func TestNoticesOfOneRequestAreSaidOneAtATime(t *testing.T) {
	const delay = 400 * time.Millisecond
	for _, scene := range []string{"one-request", "one-request-without-the-setting", "two-requests"} {
		t.Run(scene, func(t *testing.T) {
			cfg := previewStagesConfig(t)
			if scene == "one-request-without-the-setting" {
				cfg.Workflow.Preview = nil
			}
			fixture := &noticeTracker{}
			fixture.install(t, alwaysChoose("done"))
			slow := http.DefaultTransport
			useCatalogTransport(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					time.Sleep(delay)
				}
				return slow.RoundTrip(r)
			})
			root, first := noticeJob(t, chain.State{})
			second := first
			if scene == "two-requests" {
				second = filepath.Join(root, "jobs", "52")
				if err := os.MkdirAll(second, 0700); err != nil {
					t.Fatal(err)
				}
			}
			var group sync.WaitGroup
			began := time.Now()
			for i, directory := range []string{first, second} {
				group.Add(1)
				go func(i int, directory string) {
					defer group.Done()
					n := requestNotices(cfg, sourceIssue{ID: 51, Key: "EXAMPLE-51"}, directory)
					text := fmt.Sprintf("notice %d of %s", i, scene)
					if err := n.say(context.Background(), resumeNotice, text, "", time.Now().UTC(), func(noticeLog, time.Time) bool { return true }, strconv.Itoa(i)); err != nil {
						t.Errorf("notice %d: %v", i, err)
					}
				}(i, directory)
			}
			group.Wait()
			took := time.Since(began)
			if posted := len(fixture.all()); posted != 2 {
				t.Fatalf("%d notices were posted", posted)
			}
			if scene == "two-requests" {
				if took >= 2*delay-50*time.Millisecond {
					t.Fatalf("notices of two requests waited for each other: %v for two posts of %v", took, delay)
				}
			} else if took < 2*delay {
				t.Fatalf("two notices of one request were said at once: %v for two posts of %v", took, delay)
			}
		})
	}
}

// Only the preview role's process is told where to write the address, and
// the operator cannot name that variable for it. A configuration without the
// setting binds exactly as before and has no kind of notice for it.
func TestOnlyThePreviewRoleIsToldWhereToWriteTheAddress(t *testing.T) {
	cfg := previewStagesConfig(t)
	bound, err := bindRequestConfig(cfg, "/queue/jobs/51", "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range bound.Roles {
		for _, process := range role.Processes {
			file, told := process.Env[previewEnv]
			if told != (role.Name == "preview_up") {
				t.Fatalf("role %s told=%v", role.Name, told)
			}
			if told && file != filepath.Join("/queue/jobs/51", "homes", "4-0", "task-preview.txt") {
				t.Fatalf("the address file is not in the process's own home: %s", file)
			}
		}
	}
	if !slices.Contains(postableKinds(cfg), previewNotice) {
		t.Fatal("the queue does not post the address with the setting")
	}
	owned := previewStagesConfig(t)
	owned.Roles[4].Processes[0].Env = map[string]string{previewEnv: "elsewhere"}
	if _, err := bindRequestConfig(owned, "/queue/jobs/51", "EXAMPLE-51"); err == nil || !strings.Contains(err.Error(), previewEnv) {
		t.Fatalf("the operator could name the address file: %v", err)
	}
	without := previewStagesConfig(t)
	without.Workflow.Preview = nil
	plain, err := bindRequestConfig(without, "/queue/jobs/51", "EXAMPLE-51")
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range plain.Roles {
		for _, process := range role.Processes {
			if _, told := process.Env[previewEnv]; told {
				t.Fatalf("role %s was told an address file without the setting", role.Name)
			}
		}
	}
	if slices.Contains(postableKinds(without), previewNotice) {
		t.Fatal("a queue without the setting posts the address")
	}
}

// The preview roles run commands, not models, each process of theirs has a
// time limit (a take-down without one could hold every later take-down), and
// neither is the role that reports a stop; the role that puts the page up has
// one process, whose file is the one read back, and the hold for taking the
// page down, when it is written, is a whole number of hours, 1 or more.
func TestThePreviewRolesAreCommandsAndTheUpRoleHasOneProcess(t *testing.T) {
	check := func(cfg config) error {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "operator.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return run(context.Background(), []string{"--check", "--config", path}, io.Discard, io.Discard)
	}
	for _, hours := range []*int{nil, func() *int { one := 1; return &one }()} {
		cfg := previewStagesConfig(t)
		cfg.Workflow.Preview.HoldHours = hours
		if err := check(cfg); err != nil {
			t.Fatalf("a valid setting was refused (hold %v): %v", hours, err)
		}
	}
	for _, variant := range []struct{ name, want string }{{"up-launches-a-model", "model"}, {"down-launches-a-model", "model"}, {"up-has-two-processes", "one process"},
		{"up-is-the-stop-report", "intake.stop_report_role"}, {"down-is-the-stop-report", "intake.stop_report_role"},
		{"up-without-a-time-limit", "timeout_minutes"}, {"down-without-a-time-limit", "timeout_minutes"},
		{"zero-hold", "hold_hours is 0"}, {"negative-hold", "hold_hours is -1"}, {"too-long-hold", "from 1 to 2562047"}} {
		t.Run(variant.name, func(t *testing.T) {
			cfg := previewStagesConfig(t)
			switch variant.name {
			case "up-launches-a-model":
				cfg.Roles[4].Processes[0].ModelEnv = "MODEL"
			case "down-launches-a-model":
				cfg.Roles[5].Processes[0].ModelEnv = "MODEL"
			case "up-has-two-processes":
				cfg.Roles[4].Processes = append(cfg.Roles[4].Processes, chain.Process{Name: "second", TimeoutMinutes: 1, Command: []string{"/bin/sh", "-c", "exit 0"}})
			case "up-without-a-time-limit":
				cfg.Roles[4].Processes[0].TimeoutMinutes = 0
			case "down-without-a-time-limit":
				cfg.Roles[5].Processes[0].TimeoutMinutes = 0
			case "up-is-the-stop-report":
				cfg.Intake.StopReportRole = "preview_up"
			case "down-is-the-stop-report":
				cfg.Intake.StopReportRole = "preview_down"
			case "zero-hold", "negative-hold", "too-long-hold":
				hours := map[string]int{"zero-hold": 0, "negative-hold": -1, "too-long-hold": 1000000000000}[variant.name]
				cfg.Workflow.Preview.HoldHours = &hours
			}
			err := check(cfg)
			if err == nil || !strings.Contains(err.Error(), variant.want) {
				t.Fatalf("the setting was not refused: %v", err)
			}
			// The refusal says where: the setting, and the role it names.
			if !strings.Contains(err.Error(), "workflow.preview.") || !strings.Contains(variant.name, "hold") && !strings.Contains(err.Error(), "roles[") {
				t.Fatalf("the refusal does not say where: %v", err)
			}
		})
	}
}
