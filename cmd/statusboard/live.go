package main

import (
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
	info, err := os.Stat(path)
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
	file, err := os.Open(path)
	if err != nil {
		http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
		return
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(from, io.SeekStart); err != nil {
		http.Error(w, "この工程の実況は読めませんでした", http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLiveChunk))
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
			if err != nil {
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
