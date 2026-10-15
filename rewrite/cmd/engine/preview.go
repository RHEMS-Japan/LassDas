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
}

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
	if err := p.post(ctx, address, words, strconv.Itoa(len(state.History))); err != nil {
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
// exited 0, and the first line of its file is an https address without
// spaces. Otherwise the reason it did not come up, in a line.
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
	text := string(data)
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[credential]")
	}
	first, rest, _ := strings.Cut(text, "\n")
	first = strings.TrimSuffix(first, "\r")
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

// validatePreview checks what the workflow cannot see of the preview roles:
// they run the operator's commands and launch no model, and the role that
// puts the page up has one process, whose file is the one read back.
func validatePreview(cfg config) error {
	if cfg.Workflow == nil || cfg.Workflow.Preview == nil {
		return nil
	}
	preview := cfg.Workflow.Preview
	for _, role := range cfg.Roles {
		if role.Name != preview.Up && role.Name != preview.Down {
			continue
		}
		for _, process := range role.Processes {
			if process.ModelEnv != "" {
				return fmt.Errorf("the preview role %q launches a model; the preview roles run the operator's commands", role.Name)
			}
		}
		if role.Name == preview.Up && len(role.Processes) != 1 {
			return fmt.Errorf("workflow.preview.up names %q, which needs exactly one process: that process's own file is where the address is read back", role.Name)
		}
	}
	return nil
}
