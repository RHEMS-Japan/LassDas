package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"automation.internal/ticket-ingress/internal/livelog"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/ticketview"
)

// maxLiveChunk bounds one answer of live output. A reader following a step
// gets the newest part of it, not a whole agent run in one response.
const maxLiveChunk = 64 << 10

// liveStep is one step's live file as the page needs it.
type liveStep struct {
	Step      string `json:"step"`
	Stage     string `json:"stage,omitempty"`
	Bytes     int64  `json:"bytes"`
	UpdatedAt int64  `json:"updated_at_ms"`
}

// serveTicketLive lists the steps that have live output, or serves one
// step's output from an offset the caller carries.
//
// The file the engine appends to is already masked line by line as it is
// written, so a value can never be split across a cut here; what is served
// is masked again, because a view that shows a secret once has shown it.
func (s *boardServer) serveTicketLive(w http.ResponseWriter, r *http.Request, runDir, step string) {
	dir := filepath.Join(runDir, livelog.Dir)
	if step == "" {
		s.serveLiveIndex(w, dir)
		return
	}
	if !liveStepPattern.MatchString(step) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, step+".log")
	// Lstat, not Stat: the run directory is the agent's own workspace, so a
	// link planted there must not be followed out of it (review of #187).
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "この工程の実況はありません", http.StatusNotFound)
		return
	}
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	if from < 0 || from > info.Size() {
		// The file was replaced by a newer run, or the caller is ahead of
		// it: start again from what is there now.
		from = 0
	}
	if info.Size()-from > maxLiveChunk {
		from = info.Size() - maxLiveChunk
	}
	// The window must begin at a line, not at a byte the caller chose. A
	// window opened inside a masked value would serve the rest of it, and
	// the mask below would no longer recognise what it is looking at: a
	// caller who asked for from=12 got the tail of a key (review of #187).
	if from > 0 {
		aligned, err := alignToLine(file(path), from)
		if err != nil {
			http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
			return
		}
		from = aligned
	}
	handle, err := os.Open(path)
	if err != nil {
		http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
		return
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Seek(from, io.SeekStart); err != nil {
		http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(handle, maxLiveChunk))
	if err != nil {
		http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
		return
	}
	text := string(raw)
	// Whole lines only: a line still being written is left for the next
	// read, so the reader never sees half a sentence and the mask below
	// never sees half a value.
	if cut := strings.LastIndexByte(text, '\n'); cut >= 0 {
		text = text[:cut+1]
	} else {
		text = ""
	}
	next := from + int64(len(text))
	masked, _, refusal := probe.MaskSecrets(text, nil)
	if refusal != "" {
		masked = "[秘密の形 (" + refusal + ") を含むため、この部分は表示しません]\n"
	}
	writeJSON(w, struct {
		Step string `json:"step"`
		From int64  `json:"from"`
		Next int64  `json:"next"`
		Size int64  `json:"size"`
		Text string `json:"text"`
	}{Step: step, From: from, Next: next, Size: info.Size(), Text: masked})
}

func (s *boardServer) serveLiveIndex(w http.ResponseWriter, dir string) {
	entries, err := os.ReadDir(dir)
	steps := []liveStep{}
	if err == nil {
		for _, entry := range entries {
			name := strings.TrimSuffix(entry.Name(), ".log")
			if entry.IsDir() || name == entry.Name() || !liveStepPattern.MatchString(name) {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			steps = append(steps, liveStep{Step: name, Stage: ticketview.LiveStage(name), Bytes: info.Size(), UpdatedAt: info.ModTime().UnixMilli()})
		}
	}
	sort.Slice(steps, func(a, b int) bool { return steps[a].UpdatedAt > steps[b].UpdatedAt })
	writeJSON(w, struct {
		Steps []liveStep `json:"steps"`
	}{Steps: steps})
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

// file opens the live log for a second read; a path that cannot be opened
// yields a reader that fails, which alignToLine reports.
func file(path string) func() (*os.File, error) {
	return func() (*os.File, error) { return os.Open(path) }
}

// alignToLine moves an offset forward to the start of the next line, so a
// window never begins in the middle of one. Reading back at most one window
// is enough: the writer never leaves a line longer than its own flush bound.
func alignToLine(open func() (*os.File, error), from int64) (int64, error) {
	handle, err := open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Seek(from, io.SeekStart); err != nil {
		return 0, err
	}
	probe := make([]byte, maxLiveChunk)
	read, err := io.ReadFull(handle, probe)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return 0, err
	}
	index := bytes.IndexByte(probe[:read], '\n')
	if index < 0 {
		// The line has not ended yet. The offset stays where it is: moving
		// past the unfinished part would serve its tail once it does end,
		// which is the leak this alignment exists to prevent.
		return from, nil
	}
	return from + int64(index) + 1, nil
}
