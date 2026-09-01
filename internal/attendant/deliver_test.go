package attendant

import (
	"encoding/json"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
)

func TestDeliverCardKeysStayOutsideTheChainNamespace(t *testing.T) {
	for _, stage := range []string{"checks", "integrate", "promote"} {
		key := deliverCardKey("delivery_0123456789abcdef0123456789abcdef", stage)
		if _, _, _, ok := runtime.ParseChainCardKey(key); ok {
			t.Fatalf("the deliver %s card key parses as a chain card key", stage)
		}
	}
}

// Enabling the v2 delivery must never reach back through past successes,
// and every unparsable or degenerate input fails closed.
func TestDeliverObservableHonoursTheCutOff(t *testing.T) {
	cutOff := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	enabled := runtime.ChainConfig{Deliver: runtime.DeliverConfig{
		ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p",
		EnabledAfter: cutOff.Format(time.RFC3339),
	}}
	after := state.RunOverview{TerminalCode: "success", ClaimedAt: cutOff.UnixMilli() + 1}
	cases := map[string]struct {
		chain runtime.ChainConfig
		run   state.RunOverview
		want  bool
	}{
		"claimed after the cut-off":  {enabled, after, true},
		"claimed before the cut-off": {enabled, state.RunOverview{TerminalCode: "success", ClaimedAt: cutOff.UnixMilli() - 1}, false},
		"never claimed":              {enabled, state.RunOverview{TerminalCode: "success"}, false},
		"not configured":             {runtime.ChainConfig{}, after, false},
		"run did not succeed":        {enabled, state.RunOverview{TerminalCode: "cancelled", ClaimedAt: after.ClaimedAt}, false},
		"cut-off missing": {runtime.ChainConfig{Deliver: runtime.DeliverConfig{
			ChecksProfile: "c", IntegrateProfile: "i", PromoteProfile: "p",
		}}, after, false},
	}
	for name, c := range cases {
		if got := deliverObservable(c.chain, c.run); got != c.want {
			t.Errorf("%s: deliverObservable() = %v, want %v", name, got, c.want)
		}
	}
}

// The Go rule mirrors the stop rule — the requester's own comment, strictly
// after the staging report, first non-blank line exactly "Go".
func TestContainsGoComment(t *testing.T) {
	const requester = int64(7)
	const reportID = int64(100)
	comment := func(id, user int64, body string) hook.BacklogComment {
		return hook.BacklogComment{CommentID: id, UserID: user, Body: body}
	}
	cases := map[string]struct {
		comments []hook.BacklogComment
		want     bool
	}{
		"go after the report":          {[]hook.BacklogComment{comment(101, requester, "Go")}, true},
		"go with surrounding blanks":   {[]hook.BacklogComment{comment(101, requester, "\n  Go  \n詳細")}, true},
		"go before the report":         {[]hook.BacklogComment{comment(99, requester, "Go")}, false},
		"go by someone else":           {[]hook.BacklogComment{comment(101, 8, "Go")}, false},
		"go not on the first line":     {[]hook.BacklogComment{comment(101, requester, "確認しました\nGo")}, false},
		"different word":               {[]hook.BacklogComment{comment(101, requester, "GO")}, false},
		"go embedded in a sentence":    {[]hook.BacklogComment{comment(101, requester, "Go します")}, false},
		"empty comment then real go":   {[]hook.BacklogComment{comment(101, requester, "\n\n"), comment(102, requester, "Go")}, true},
		"exactly the report id itself": {[]hook.BacklogComment{comment(reportID, requester, "Go")}, false},
	}
	for name, c := range cases {
		if got := containsGoComment(c.comments, requester, reportID); got != c.want {
			t.Errorf("%s: containsGoComment() = %v, want %v", name, got, c.want)
		}
	}
}

func TestCommentIDWithMarkerFindsTheExactReport(t *testing.T) {
	marker := hook.CommentMarker(string(hook.RunCommentStagingReport), "run_20260901_abcdefabcdefabcdefabcdef")
	comments := []hook.BacklogComment{
		{CommentID: 1, Body: "ふつうのコメント"},
		{CommentID: 2, Body: "本文\n" + marker},
		{CommentID: 3, Body: "別の run の報告\n" + hook.CommentMarker(string(hook.RunCommentStagingReport), "run_20260901_000000000000000000000000")},
	}
	id, found := commentIDWithMarker(comments, marker)
	if !found || id != 2 {
		t.Fatalf("commentIDWithMarker() = %d, %v", id, found)
	}
	if _, found := commentIDWithMarker(comments[:1], marker); found {
		t.Fatal("a marker was found where none exists")
	}
	// A requester-forged copy of the marker BEFORE the real report must not
	// lower the bar: the newest occurrence wins.
	forged := append([]hook.BacklogComment{{CommentID: 1, Body: "自作\n" + marker}}, comments[1:]...)
	if id, found := commentIDWithMarker(forged, marker); !found || id != 2 {
		t.Fatalf("forged-first commentIDWithMarker() = %d, %v (want the newest, 2)", id, found)
	}
}

func TestDeliverPreviewRendersHonestBounds(t *testing.T) {
	if preview := deliverPreview(nil); !preview.Unavailable {
		t.Fatal("a missing delta did not render as unavailable")
	}
	if preview := deliverPreview(json.RawMessage(`{"status":"unavailable"}`)); !preview.Unavailable {
		t.Fatal("an unavailable delta did not render as unavailable")
	}
	raw := json.RawMessage(`{"status":"ahead","ahead_by":3,"commits":[{"title":"feat: A"},{"title":"fix: B"},{"title":"docs: C"}]}`)
	preview := deliverPreview(raw)
	if preview.Unavailable || preview.AheadBy != 3 || len(preview.Titles) != 3 || preview.Titles[0] != "feat: A" {
		t.Fatalf("preview = %+v", preview)
	}
	big := `{"status":"ahead","ahead_by":30,"commits":[`
	for index := 0; index < 30; index++ {
		if index > 0 {
			big += ","
		}
		big += `{"title":"c"}`
	}
	big += `]}`
	capped := deliverPreview(json.RawMessage(big))
	if len(capped.Titles) != 20 || !capped.Truncated {
		t.Fatalf("capped preview = %d titles, truncated=%v", len(capped.Titles), capped.Truncated)
	}
}
