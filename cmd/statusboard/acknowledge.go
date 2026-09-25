package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Clearing a finished card away is a decision a person makes, and this file
// is where the board remembers they made it.
//
// The board used to move a card to 完了・終了 the moment the attendant called
// the run finished. Nothing recorded when that was, and the card said only
// what it had ended as, so anyone coming back to the board could not tell a
// run that ended a minute ago from one that ended the day before — and
// nobody had looked at either. A finished run now stays in the running lane,
// showing when it got there, until a person presses 確認して片付ける; that
// press moves it down and is itself stamped with the time.
//
// The acknowledgement is the board's own fact: it goes to no tracker, needs
// no requester credential, and is written into the status directory beside
// the board's action journal — never into the attendant's snapshot, which
// has exactly one writer.

// acknowledgeFileName is the board's record of which finished runs a person
// has cleared away. Its own file, disjoint from the attendant's board.json
// and events.jsonl, so the single-writer rule holds per file.
const acknowledgeFileName = "acknowledged.json"

// maxAcknowledgements bounds the record. The snapshot shows a few dozen runs
// at a time and older ones leave it for good, so entries past this many are
// for runs no board will ever show again; the oldest go first.
const maxAcknowledgements = 500

// maxAcknowledgeFileBytes bounds one reading of the record, the way every
// other file this board reads is bounded.
const maxAcknowledgeFileBytes = 1 << 20

// acknowledgement is one person's "I have seen this one, take it off the
// list", with when and who, because the whole point is being able to say
// when.
type acknowledgement struct {
	At       time.Time `json:"at"`
	User     string    `json:"user,omitempty"`
	ClientIP string    `json:"client_ip,omitempty"`
}

type acknowledgeRecord struct {
	SchemaVersion int                        `json:"schema_version"`
	Runs          map[string]acknowledgement `json:"runs"`
}

// readAcknowledgements returns what the board has cleared away, keyed by
// delivery. An absent, oversized or unreadable record is no acknowledgement
// at all: every card then stays in the running lane, which is the state that
// asks a person to look rather than the one that hides things.
func readAcknowledgements(statusDir string) map[string]acknowledgement {
	raw, err := os.ReadFile(filepath.Join(statusDir, acknowledgeFileName))
	if err != nil || len(raw) > maxAcknowledgeFileBytes {
		return map[string]acknowledgement{}
	}
	var record acknowledgeRecord
	if json.Unmarshal(raw, &record) != nil || record.Runs == nil {
		return map[string]acknowledgement{}
	}
	for id, entry := range record.Runs {
		if id == "" || entry.At.IsZero() {
			delete(record.Runs, id)
		}
	}
	return record.Runs
}

// acknowledgeRun writes down that this delivery was cleared away, and
// returns the acknowledgement that now stands. Pressing twice is not two
// decisions: the first time is kept, so the time on the card is when the
// person actually looked.
func (s *boardServer) acknowledgeRun(deliveryID string, entry acknowledgement) (acknowledgement, error) {
	s.ackMu.Lock()
	defer s.ackMu.Unlock()
	runs := readAcknowledgements(s.statusDir)
	if existing, seen := runs[deliveryID]; seen {
		return existing, nil
	}
	runs[deliveryID] = entry
	if len(runs) > maxAcknowledgements {
		ids := make([]string, 0, len(runs))
		for id := range runs {
			ids = append(ids, id)
		}
		// Oldest first, and the id breaks a tie so two entries stamped in
		// the same instant are dropped in a fixed order.
		sort.Slice(ids, func(a, b int) bool {
			if runs[ids[a]].At.Equal(runs[ids[b]].At) {
				return ids[a] < ids[b]
			}
			return runs[ids[a]].At.Before(runs[ids[b]].At)
		})
		for _, id := range ids[:len(runs)-maxAcknowledgements] {
			delete(runs, id)
		}
	}
	encoded, err := json.Marshal(acknowledgeRecord{SchemaVersion: 1, Runs: runs})
	if err != nil {
		return acknowledgement{}, err
	}
	// Whole file at once, then renamed over the old one: a reader that
	// arrives mid-write sees one complete record or the other, never half.
	temp := filepath.Join(s.statusDir, acknowledgeFileName+".tmp")
	if err := os.WriteFile(temp, encoded, 0o600); err != nil {
		return acknowledgement{}, err
	}
	if err := os.Rename(temp, filepath.Join(s.statusDir, acknowledgeFileName)); err != nil {
		_ = os.Remove(temp)
		return acknowledgement{}, err
	}
	return entry, nil
}

// finishedBoardRun finds the row a person is clearing away. Only a delivery
// the board is showing right now, and only one that has stopped for good: a
// running card has no finished state to acknowledge, and an id the board
// does not show is not a row this record should grow an entry for.
func (s *boardServer) finishedBoardRun(deliveryID string) (boardRun, string) {
	if deliveryID == "" {
		return boardRun{}, "どの依頼を片付けるのか指定されていません"
	}
	raw, err := os.ReadFile(filepath.Join(s.statusDir, "board.json"))
	if err != nil {
		return boardRun{}, "盤面の状態を読めないため、操作を受け付けられません"
	}
	var board struct {
		Runs []boardRun `json:"runs"`
	}
	if json.Unmarshal(raw, &board) != nil {
		return boardRun{}, "盤面の状態を読めないため、操作を受け付けられません"
	}
	for _, run := range board.Runs {
		if run.DeliveryID != deliveryID {
			continue
		}
		if !finishedStep(run.Step) {
			return boardRun{}, "この依頼はまだ終わっていません。終了してから片付けてください"
		}
		return run, ""
	}
	return boardRun{}, "表示した実行を盤面で確認できません。画面を更新して現在の状態を確認してください"
}

// finishedStep is the board's copy of the attendant's register of states a
// run rests in for good. The lane a card sits in is decided here and in the
// page, never from the attendant's side: what is finished is the engine's
// word, what has been cleared away is the board's.
func finishedStep(step string) bool {
	return step == "done" || step == "stopped" || step == "failed"
}

type acknowledgeRequest struct {
	DeliveryID string `json:"delivery_id"`
}

type acknowledgeResponse struct {
	DeliveryID     string    `json:"delivery_id"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// serveAcknowledge takes one card off the running lane. It posts nothing and
// carries nobody's authority but the board user's own, so it is open on a
// board with no requester credential — the state a board is in for most of
// its life. Cross-site writes are closed the same two ways the tracker
// actions close them: a JSON content type an HTML form cannot send, and an
// Origin check when the header is there.
func (s *boardServer) serveAcknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
		return
	}
	if s.readOnly {
		http.Error(w, "このボードは閲覧専用で起動しています", http.StatusForbidden)
		return
	}
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		http.Error(w, "JSON のみ", http.StatusUnsupportedMediaType)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != r.Host {
			http.Error(w, "送信元が不正です", http.StatusForbidden)
			return
		}
	}
	var request acknowledgeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request); err != nil {
		http.Error(w, "リクエストが不正です", http.StatusBadRequest)
		return
	}
	run, denied := s.finishedBoardRun(request.DeliveryID)
	if denied != "" {
		http.Error(w, denied, http.StatusForbidden)
		return
	}
	boardUser, _, _ := r.BasicAuth()
	entry, err := s.acknowledgeRun(run.DeliveryID, acknowledgement{
		At: time.Now().UTC(), User: boardUser, ClientIP: clientIP(r),
	})
	if err != nil {
		s.logger.Error("acknowledgement could not be written down", "delivery", run.DeliveryID, "error", err.Error())
		http.Error(w, "片付けた記録を書けませんでした。時間をおいて再試行してください", http.StatusInternalServerError)
		return
	}
	s.logger.Info("finished run acknowledged", "delivery", run.DeliveryID, "user", boardUser, "client_ip", entry.ClientIP)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(acknowledgeResponse{DeliveryID: run.DeliveryID, AcknowledgedAt: entry.At})
}
