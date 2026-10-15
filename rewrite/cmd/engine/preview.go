package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"ticket-runner/internal/chain"
	"ticket-runner/internal/textclip"
)

// A page where the requester sees the change before the question that
// confirms it. What puts the page up and takes it down is the operator's: the
// two roles workflow.preview names. The runtime decides when they run, keeps
// its own record of what came of it and posts the address in its own words.
// It reads nothing a role prints, and nothing that comes of the page fails,
// repeats or satisfies a stage.

// previewEnv names the file, in the up role's own home, where that role
// writes the page's address on the first line and any words for the
// requester after it.
const previewEnv = "TASK_PREVIEW"

// previewNotice is the kind of the runtime's comment with the address.
const previewNotice = "preview"

// previewFile is the request's own record of its page: whether a launch of
// the up role may have left something to take down. It sits beside the
// request's notices, not in its checkout.
const previewFile = "preview.json"

// previewBytes bounds the address file, as the activity record is bounded.
const previewBytes = 16 << 10

// previewRecord is what the request's record says of its page. State is
// starting while the up role runs, up once an address was read back, and
// failed when none was: a failed launch may still have made part of a page.
type previewRecord struct {
	State     string    `json:"state"`
	Role      string    `json:"role"`
	StartedAt time.Time `json:"started_at"`
	URL       string    `json:"url,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	// DownAt is when the down role exited 0, with State down. A State of
	// unconfirmed is a page the hold ran out on before that.
	DownAt *time.Time `json:"down_at,omitempty"`
	// Refused counts the down role's launches in a row that did not exit 0,
	// DownReason keeps the last one's reason, and Next is when it is
	// launched again.
	Refused    int       `json:"refused,omitempty"`
	DownReason string    `json:"down_reason,omitempty"`
	Next       time.Time `json:"next,omitzero"`
}

// previewDownLock is held while the down role runs for a request.
const previewDownLock = "preview-down/runner.lock"

func writePreviewRecord(job string, record previewRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeRuntimeFile(filepath.Join(job, previewFile), data)
}

// What the runtime tells the question chosen after the change was read,
// added to its own instruction for that question. Plain words, not a format.
func previewPostedInstruction(address string) string {
	return "The runtime has put this change up for the requester to see at " + address + " and has posted that address to the requester in its own comment, right before this question. In the question, ask the requester to open that address to see the change. Do not write that you opened the page or checked it yourself.\n"
}

func previewUnconfirmedInstruction(address string) string {
	return "The runtime has put this change up for the requester to see at " + address + ", but could not confirm that its own comment with that address was posted. Give that address in the question and ask the requester to open it to see the change. Do not write that you opened the page or checked it yourself.\n"
}

const previewFailedInstruction = "The runtime could not put this change up for the requester to see. Say so in one line of the question, 確認用の画面は用意できなかった, and describe the change in words as before.\n"

// previewAssignment is what the up role's launch is told. The role is the
// operator's command; nothing it prints is read.
const previewAssignment = "Put the change in the checkout up for the requester to see. Write the page's https address on the first line of the file TASK_PREVIEW names, and any words for the requester after it. Exit 0 only when the page answers at that address.\n"

// previewer puts the page up for one accepted request, with the same
// launching the request's stages have.
type previewer struct {
	cfg      config
	executor chain.Executor
	// job is the request's directory, where its record and notices are.
	job, key string
	observe  func(string)
}

// up launches the up role once and returns the runtime's note of what came of
// it and the words for the question. Before the launch it removes the address
// the previous launch left, so that one is never read as this launch's, and
// records that a page is going up, so a stop or restart in the middle still
// leaves it to be taken down.
func (p previewer) up(ctx context.Context, role string, state chain.State) (string, string) {
	failed := func(reason string) (string, string) {
		return "確認用の画面を用意できなかった: " + reason, previewFailedInstruction
	}
	file := ""
	for _, configured := range p.cfg.Roles {
		if configured.Name == role && len(configured.Processes) == 1 {
			file = configured.Processes[0].Env[previewEnv]
		}
	}
	if file == "" {
		return failed("the role was given no " + previewEnv + " file to write the address in, so it was not launched")
	}
	if err := os.Remove(file); err != nil && !errors.Is(err, os.ErrNotExist) {
		return failed("the address the previous launch left could not be removed, so the role was not launched: " + err.Error())
	}
	record := previewRecord{State: "starting", Role: role, StartedAt: time.Now().UTC()}
	if err := writePreviewRecord(p.job, record); err != nil {
		return failed("the request's record of the page could not be written, so the role was not launched: " + err.Error())
	}
	results := p.executor.Execute(ctx, chain.Assignment{Role: role, Instruction: previewAssignment}, state)
	address, words, reason := previewOutcome(results, file, credentialValues(p.cfg))
	if reason != "" {
		record.State, record.Reason = "failed", reason
		p.keep(record)
		return failed(reason)
	}
	record.State, record.URL = "up", address
	p.keep(record)
	if err := p.post(ctx, address, words, previewPoint(state.History)); err != nil {
		reason := textclip.Clip(firstInstructionLine(scrubCredentials(p.cfg, err.Error())), 300)
		return "確認用の画面を用意した: " + address + "。アドレスを知らせる本体のコメントは投稿を確かめられなかった: " + reason,
			previewUnconfirmedInstruction(address)
	}
	return "確認用の画面を用意し、本体のコメントでアドレスを知らせた: " + address, previewPostedInstruction(address)
}

// keep writes the page's record as it now stands. A record that could not be
// written still says a page was going up, which is taken down all the same.
func (p previewer) keep(record previewRecord) {
	if err := writePreviewRecord(p.job, record); err != nil && p.observe != nil {
		p.observe("the record of the page put up for the requester was not updated: " + err.Error())
	}
}

// previewPoint names the point of the record where the question was chosen:
// how many records it holds that are not the runtime's own notes about its
// decisions. A decision that was unavailable, after a restart that cut a
// put-up short, adds such a note and does not move the point; the work and
// the requester's words that come before a later question do.
func previewPoint(history []chain.Result) string {
	count := 0
	for _, record := range history {
		if record.Role != "router" {
			count++
		}
	}
	return strconv.Itoa(count)
}

// post says the address once for this point of the record, in the runtime's
// own words, before the question. A restart that puts the page up again at
// the same point does not say it twice; a notice recorded and never confirmed
// is settled first, as every notice is.
func (p previewer) post(ctx context.Context, address, words, event string) error {
	raw, err := os.ReadFile(filepath.Join(p.job, "issue.json"))
	if err != nil {
		return err
	}
	issue, err := p.cfg.source().ReadIssue(raw)
	if err != nil {
		return err
	}
	if issue.Key != p.key {
		return errors.New("the request's saved issue is another issue")
	}
	text := "確認用の画面: " + address
	if words != "" {
		text += "\n" + words
	}
	n := requestNotices(p.cfg, issue, p.job)
	said := func(record noticeRecord) bool { return record.Kind == previewNotice && record.Event == event }
	due := func(log noticeLog, _ time.Time) bool { return !slices.ContainsFunc(log.Notices, said) }
	if err := n.say(ctx, previewNotice, text, "", time.Now().UTC(), due, event); err != nil {
		return err
	}
	log, err := n.load()
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(log.Notices, func(record noticeRecord) bool { return said(record) && record.PostedAt != nil }) {
		return errors.New("the comment with the address is not confirmed")
	}
	return nil
}

// previewOutcome reads what one launch of the up role left: every process
// exited 0, its file is text without control characters other than line
// breaks, and the first line is an https address without spaces. Otherwise
// the reason it did not come up, in a line.
func previewOutcome(results []chain.Result, file string, secrets []string) (address, words, reason string) {
	if len(results) == 0 {
		return "", "", "the launch returned no result"
	}
	for _, result := range results {
		if result.Error != "" {
			return "", "", launchReason(result.Error)
		}
	}
	data, reason := readPreviewAddress(file)
	if reason != "" {
		return "", "", reason
	}
	// The words go to the requester as they are, so the file is text for a
	// person: Windows line ends are read as line breaks, and any other
	// control character, on any line, is a file that is not.
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.ContainsFunc(text, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) {
		return "", "", "the address file holds a control character other than a line break"
	}
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[credential]")
	}
	first, rest, _ := strings.Cut(text, "\n")
	if !previewAddressLine(first) {
		return "", "", "the first line the role wrote is not an https:// address without spaces"
	}
	return first, strings.TrimSpace(rest), ""
}

// previewAddressLine is an https address with nothing else on the line.
func previewAddressLine(line string) bool {
	if !strings.HasPrefix(line, "https://") || len(line) == len("https://") || !utf8.ValidString(line) {
		return false
	}
	return !strings.ContainsFunc(line, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

// readPreviewAddress reads the role's file as the activity record is read:
// without following a link or waiting on a pipe, an ordinary file with one
// name and no more than the bound, in UTF-8.
func readPreviewAddress(path string) ([]byte, string) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "the role exited 0 and wrote no address"
	}
	if err != nil {
		return nil, "the address file is not an ordinary file: " + err.Error()
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > previewBytes {
		return nil, fmt.Sprintf("the address file is not an ordinary file of at most %d bytes", previewBytes)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return nil, "the address file has another name"
	}
	data, err := io.ReadAll(io.LimitReader(file, previewBytes+1))
	if err != nil || len(data) > previewBytes || !utf8.Valid(data) {
		return nil, "the address file could not be read as UTF-8 text within the bound"
	}
	return data, ""
}

// launchReason says in a line why a launch did not exit 0: the time limit by
// that name, an exit status with the last line the process wrote.
func launchReason(text string) string {
	line := firstInstructionLine(text)
	switch {
	case strings.HasPrefix(line, context.DeadlineExceeded.Error()):
		line = "stopped at its time limit"
	case strings.HasPrefix(line, "exit status ") || strings.HasPrefix(line, "signal: "):
		if last := lastInstructionLine(text); last != "" && last != line {
			line += ": " + last
		}
	}
	return textclip.Clip(line, 300)
}

// scrubCredentials replaces every credential value the controller can name.
func scrubCredentials(cfg config, text string) string {
	for _, value := range credentialValues(cfg) {
		text = strings.ReplaceAll(text, value, "[credential]")
	}
	return text
}

// defaultPreviewHoldHours is the hold when workflow.preview.hold_hours is
// left out: a page lifetime of 72 hours plus one.
const defaultPreviewHoldHours = 73

// previewHold is how long after a page was put up its request's caches and
// stopped workspace are kept for the down role.
func previewHold(preview *chain.Preview) time.Duration {
	hours := defaultPreviewHoldHours
	if preview.HoldHours != nil {
		hours = *preview.HoldHours
	}
	return time.Duration(hours) * time.Hour
}

// validatePreview checks what the workflow cannot see of the preview roles:
// they run the operator's commands and launch no model, the role that puts
// the page up has one process, whose file is the one read back, and the hold
// for taking the page down is a positive number of hours.
func validatePreview(cfg config) error {
	if cfg.Workflow == nil || cfg.Workflow.Preview == nil {
		return nil
	}
	preview := cfg.Workflow.Preview
	if hold := preview.HoldHours; hold != nil && (*hold < 1 || uint64(*hold) > uint64(time.Duration(1<<63-1)/time.Hour)) {
		return fmt.Errorf("workflow.preview.hold_hours is %d: it is a whole number of hours from 1 to %d (the page's lifetime plus one), or left out for %d", *hold, uint64(time.Duration(1<<63-1)/time.Hour), defaultPreviewHoldHours)
	}
	for i, role := range cfg.Roles {
		setting := "workflow.preview.up"
		switch role.Name {
		case preview.Up:
		case preview.Down:
			setting = "workflow.preview.down"
		default:
			continue
		}
		for j, process := range role.Processes {
			if process.ModelEnv != "" {
				return fmt.Errorf("%s names %q, whose process %q (roles[%d].processes[%d]) sets model_env: the preview roles run the operator's commands and launch no model", setting, role.Name, process.Name, i, j)
			}
		}
		if cfg.Intake != nil && role.Name == cfg.Intake.StopReportRole {
			return fmt.Errorf("%s names %q (roles[%d]), which is also intake.stop_report_role: the report after a stop and the take-down would run the same commands twice", setting, role.Name, i)
		}
		if role.Name == preview.Up && len(role.Processes) != 1 {
			return fmt.Errorf("workflow.preview.up names %q (roles[%d]), which has %d processes and needs exactly one process: that process's own file is where the address is read back", role.Name, i, len(role.Processes))
		}
	}
	return nil
}

// previewDownName begins the launch name of a take-down, followed by the
// request's own name. One runs at a time, apart from the requests' slots.
const previewDownName = "preview-down:"

// previewDownAssignment is what the down role's launch is told. The role is
// the operator's command; nothing it prints is read.
const previewDownAssignment = "Take down the page put up for the requester of this request. Exit 0 only when nothing of it is left.\n"

// readPreviewRecord reads the request's record of its page.
func readPreviewRecord(job string) (previewRecord, error) {
	var record previewRecord
	raw, err := os.ReadFile(filepath.Join(job, previewFile))
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return previewRecord{}, errors.New("the record of the page is unreadable")
	}
	return record, nil
}

// pageDowns takes down the pages of delivered and stopped requests for the
// queue. A take-down runs in a launch of its own, so the queue's other
// requests are not held up while it runs, and it takes none of their slots.
type pageDowns struct {
	ctx    context.Context
	cfg    config
	active map[string]bool
	launch func(string, func() error)
	// trouble keeps, per request, the last unreadable record said.
	trouble map[string]string
}

// settle launches the down role for a delivered or stopped request whose
// page may be up, once it is due and no other take-down runs, and says
// whether the request's caches and stopped workspace may now be cleared: the
// request never put a page up, its page is down, or the hold ran out.
func (d pageDowns) settle(issue sourceIssue, directory, name string, say func(string)) bool {
	configured := d.cfg.Workflow != nil && d.cfg.Workflow.Preview != nil
	record, err := readPreviewRecord(directory)
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	if !configured {
		// The operator removed the setting: there is no role to take the
		// page down, and nothing waits for one.
		if err == nil && (record.State == "down" || record.State == "unconfirmed") {
			return true
		}
		const unset = "workflow.preview is not set, so the page put up for the requester is not taken down; the operator's own cleanup removes it at the end of its lifetime"
		if d.trouble[name] != unset {
			say(unset)
			d.trouble[name] = unset
		}
		return true
	}
	if err != nil {
		if d.trouble[name] != err.Error() {
			say(err.Error() + "; the request's caches and workspace are not held for its page")
			d.trouble[name] = err.Error()
		}
		return true
	}
	if record.State == "down" || record.State == "unconfirmed" {
		return true
	}
	launch := previewDownName + name
	if d.active[launch] {
		return false
	}
	if !turnClock().Before(record.StartedAt.Add(previewHold(d.cfg.Workflow.Preview))) {
		// The operator's own cleanup has had the page's lifetime. Kept any
		// longer, a stopped workspace would never be reclaimed while the
		// take-down cannot answer, and once it is the role has nowhere to run.
		record.State = "unconfirmed"
		if err := writePreviewRecord(directory, record); err != nil {
			say("the record of the page was not updated: " + err.Error())
			return false
		}
		say("the page put up for the requester was not confirmed taken down within workflow.preview.hold_hours of being put up; the request's caches and workspace are no longer held for it")
		return true
	}
	for running := range d.active {
		if strings.HasPrefix(running, previewDownName) {
			return false
		}
	}
	if record.Refused > 0 && turnClock().Before(record.Next) {
		return false
	}
	d.launch(launch, func() error {
		d.takeDown(issue, directory, say)
		return nil
	})
	return false
}

// takeDown launches the down role once and records what came of it: the time
// the page went down, or the refusal with when to try again. A reason is said
// once while it stays the same, as a refused status is.
func (d pageDowns) takeDown(issue sourceIssue, directory string, say func(string)) {
	err := runPreviewDown(d.ctx, d.cfg, issue, directory)
	if d.ctx.Err() != nil {
		// The runtime is stopping; this launch was not a refusal.
		return
	}
	record, readErr := readPreviewRecord(directory)
	if readErr != nil {
		say(readErr.Error() + "; what came of taking the page down was not recorded")
		return
	}
	if err == nil {
		now := time.Now().UTC()
		record.State, record.DownAt = "down", &now
		record.Refused, record.DownReason, record.Next = 0, "", time.Time{}
		if err := writePreviewRecord(directory, record); err != nil {
			say("the page put up for the requester was taken down but that was not recorded: " + err.Error())
			return
		}
		say("the page put up for the requester was taken down")
		return
	}
	reason := scrubCredentials(d.cfg, err.Error())
	record.Refused++
	record.Next = turnClock().Add(retryDelay(record.Refused))
	if record.DownReason != reason {
		say("the page put up for the requester was not taken down, tried again later: " + reason)
	}
	record.DownReason = reason
	if err := writePreviewRecord(directory, record); err != nil {
		say("the record of the page was not updated: " + err.Error())
	}
}

// runPreviewDown launches the down role once for the request, bound to its
// own home, workspace and issue as its other roles are, while holding the
// lock that tells the reclaim of a stopped workspace the role is running.
func runPreviewDown(ctx context.Context, cfg config, issue sourceIssue, directory string) error {
	if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(previewDownLock)), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(directory, previewDownLock), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another process is taking this request's page down")
	}
	bound, err := bindRequestConfig(cfg, directory, issue.Key)
	if err != nil {
		return err
	}
	roles := map[string]chain.Role{}
	for _, role := range bound.Roles {
		if role.Name == cfg.Workflow.Preview.Down {
			roles[role.Name] = role
		}
	}
	if len(roles) == 0 {
		return errors.New("workflow.preview.down names no configured role")
	}
	prepare, err := roleAccess(bound, issue.Key)
	if err != nil {
		return err
	}
	executor := chain.Processes{Roles: roles, Prepare: prepare}
	results := executor.Execute(ctx, chain.Assignment{Role: cfg.Workflow.Preview.Down, Instruction: previewDownAssignment},
		chain.State{Request: "The page put up for the requester of " + issue.Key + "."})
	if len(results) == 0 {
		return errors.New("the launch returned no result")
	}
	for _, result := range results {
		if result.Error != "" {
			return errors.New(launchReason(result.Error))
		}
	}
	return nil
}
