package ticketview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A design-backed delivery records, per round, the investigation (what was
// measured and found), the design (cause, approach, files, what is left
// alone), each design reviewer's verdict, the round's decision and - when
// the applier could not follow the design - its objection. All of it is
// read here; nothing is invented for a file that is missing.
func (v *View) readDesignRounds(runDir string) {
	for n := 1; n <= 20; n++ {
		dir := filepath.Join(runDir, "history", fmt.Sprintf("design-%d", n))
		if !exists(dir) {
			break
		}
		v.readInvestigation(dir, n)
		v.readDesign(dir, n)
		v.readDesignReviews(dir, n)
		v.readObjection(dir, n)
	}
}

func (v *View) readInvestigation(dir string, n int) {
	path := filepath.Join(dir, "investigation.json")
	var investigation struct {
		ProbesUsed     int      `json:"probes_used"`
		ElapsedSeconds int      `json:"elapsed_seconds"`
		Questions      []string `json:"questions"`
		Findings       []struct {
			Claim      string   `json:"claim"`
			Evidence   []string `json:"evidence"`
			Confidence string   `json:"confidence"`
		} `json:"findings"`
		Unknowns []string `json:"unknowns"`
		Next     string   `json:"next"`
	}
	if !readJSON(path, &investigation) {
		return
	}
	event := Event{
		At: fileTime(path), Step: "investigate", Tone: "ok",
		Title:  fmt.Sprintf("調査 %d 巡目: 発見 %d 件・不明 %d 件 (計測 %d 回)", n, len(investigation.Findings), len(investigation.Unknowns), investigation.ProbesUsed),
		Record: fmt.Sprintf("design-%d-investigation", n),
	}
	if investigation.Next != "" {
		event.Why = shown(investigation.Next)
	}
	if investigation.ElapsedSeconds > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "所要", Text: (time.Duration(investigation.ElapsedSeconds) * time.Second).String()})
	}
	for i, f := range investigation.Findings {
		text := f.Claim
		if len(f.Evidence) > 0 {
			text += "\n根拠: " + strings.Join(f.Evidence, " / ")
		}
		event.Evidence = append(event.Evidence, Evidence{Label: fmt.Sprintf("発見 %d (%s)", i+1, f.Confidence), Text: shown(text)})
	}
	for i, u := range investigation.Unknowns {
		event.Evidence = append(event.Evidence, Evidence{Label: fmt.Sprintf("不明 %d", i+1), Text: shown(u)})
	}
	for i, q := range investigation.Questions {
		event.Evidence = append(event.Evidence, Evidence{Label: fmt.Sprintf("問い %d", i+1), Text: shown(q)})
	}
	v.Timeline = append(v.Timeline, event)
}

func (v *View) readDesign(dir string, n int) {
	path := filepath.Join(dir, "design.json")
	var design struct {
		Cause         string   `json:"cause"`
		CauseEvidence []string `json:"cause_evidence"`
		Approach      string   `json:"approach"`
		Alternatives  []string `json:"alternatives"`
		Files         []struct {
			Path    string   `json:"path"`
			Changes []string `json:"changes"`
		} `json:"files"`
		Verification struct {
			Form         string `json:"form"`
			Path         string `json:"path"`
			ExpectedText string `json:"expected_text"`
			AbsentText   string `json:"absent_text"`
			Probe        string `json:"probe"`
			Metric       string `json:"metric"`
		} `json:"verification"`
		BlastRadius []string `json:"blast_radius"`
		NotDoing    []string `json:"not_doing"`
	}
	if !readJSON(path, &design) {
		return
	}
	event := Event{
		At: fileTime(path), Step: "design", Tone: "ok",
		Title:  fmt.Sprintf("設計 %d 巡目: 触るファイル %d 件", n, len(design.Files)),
		Why:    shown(design.Approach),
		Record: fmt.Sprintf("design-%d-design", n),
	}
	if design.Cause != "" {
		text := design.Cause
		if len(design.CauseEvidence) > 0 {
			text += "\n根拠: " + strings.Join(design.CauseEvidence, " / ")
		}
		event.Evidence = append(event.Evidence, Evidence{Label: "原因", Text: shown(text)})
	}
	for _, f := range design.Files {
		event.Evidence = append(event.Evidence, Evidence{Label: "変更: " + f.Path, Text: shown(strings.Join(f.Changes, "\n"))})
	}
	if design.Verification.Form != "" {
		parts := []string{design.Verification.Form}
		for _, part := range []string{design.Verification.Path, design.Verification.Probe, design.Verification.Metric, design.Verification.ExpectedText, design.Verification.AbsentText} {
			if part != "" {
				parts = append(parts, part)
			}
		}
		event.Evidence = append(event.Evidence, Evidence{Label: "検証のしかた", Text: shown(strings.Join(parts, " · "))})
	}
	if len(design.Alternatives) > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "退けた案", Text: shown(strings.Join(design.Alternatives, "\n"))})
	}
	if len(design.BlastRadius) > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "影響の範囲", Text: shown(strings.Join(design.BlastRadius, "\n"))})
	}
	if len(design.NotDoing) > 0 {
		event.Evidence = append(event.Evidence, Evidence{Label: "やらないこと", Text: shown(strings.Join(design.NotDoing, "\n"))})
	}
	v.Timeline = append(v.Timeline, event)
}

func (v *View) readDesignReviews(dir string, n int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var decision struct {
		Outcome string `json:"outcome"`
	}
	readJSON(filepath.Join(dir, "decision.json"), &decision)
	const suffix = "-design-review.json"
	for _, entry := range entries {
		name := entry.Name()
		reviewer, isReview := strings.CutSuffix(name, suffix)
		if !isReview || !recordBase.MatchString(reviewer) {
			continue
		}
		var review struct {
			ReviewerID string    `json:"reviewer_id"`
			Model      string    `json:"model"`
			Lens       string    `json:"lens"`
			Verdict    string    `json:"verdict"`
			ReviewedAt time.Time `json:"reviewed_at"`
			Findings   []struct {
				Code    string `json:"code"`
				Section string `json:"section"`
				Message string `json:"message"`
			} `json:"findings"`
			Invocation invocation `json:"invocation"`
		}
		if !readJSON(filepath.Join(dir, name), &review) || review.Verdict == "" {
			continue
		}
		id := review.ReviewerID
		if id == "" {
			id = reviewer
		}
		v.addCost(fmt.Sprintf("設計レビュー %d 巡目 (%s)", n, id), review.Invocation.CostUSD)
		tone, title := "ok", fmt.Sprintf("設計レビュー %d 巡目 · %s: 合格", n, id)
		if review.Verdict != "pass" {
			tone, title = "warn", fmt.Sprintf("設計レビュー %d 巡目 · %s: %s (指摘 %d 件)", n, id, verdictWord(review.Verdict), len(review.Findings))
		}
		event := Event{At: review.ReviewedAt, Step: "design-review", Tone: tone, Title: title, Record: fmt.Sprintf("design-%d-%s-design-review", n, reviewer)}
		for _, f := range review.Findings {
			event.Evidence = append(event.Evidence, Evidence{Label: f.Code + " (" + f.Section + ")", Text: shown(f.Message)})
		}
		if review.Lens != "" {
			event.Evidence = append(event.Evidence, Evidence{Label: "観点", Text: shown(review.Lens)})
		}
		if review.Model != "" {
			event.Evidence = append(event.Evidence, Evidence{Label: "モデル", Text: review.Model})
		}
		v.Timeline = append(v.Timeline, event)
	}
	// A design reviewer whose run record exists without a sealed verdict
	// did not answer; the tail of its transcript is the only evidence.
	for _, entry := range entries {
		name := entry.Name()
		reviewer, isRun := strings.CutSuffix(name, "-design-review-run.json")
		if !isRun || !recordBase.MatchString(reviewer) || exists(filepath.Join(dir, reviewer+suffix)) {
			continue
		}
		var run struct {
			RanAt      time.Time `json:"ran_at"`
			Transcript string    `json:"transcript"`
		}
		if !readJSON(filepath.Join(dir, name), &run) {
			continue
		}
		v.Timeline = append(v.Timeline, Event{
			At: run.RanAt, Step: "design-review", Tone: "bad", Title: fmt.Sprintf("設計レビュー %d 巡目 · %s: 判定を返せなかった", n, reviewer),
			Evidence: []Evidence{{Label: "レビュー役の出力の末尾", Text: shownTail(run.Transcript, 800)}},
			Record:   fmt.Sprintf("design-%d-%s-design-review-run", n, reviewer),
		})
	}
	if decision.Outcome != "" {
		last := len(v.Timeline) - 1
		if last >= 0 && v.Timeline[last].Step == "design-review" {
			v.Timeline[last].Why = "この巡の結論: " + designOutcomeWord(decision.Outcome)
		}
	}
}

func designOutcomeWord(outcome string) string {
	switch outcome {
	case "approved":
		return "承認 (実装へ)"
	case "revise":
		return "差し戻し (設計をやり直す)"
	case "nonconverged":
		return "収束せず (設計の巡回を使い切った)"
	}
	return outcome
}

func (v *View) readObjection(dir string, n int) {
	path := filepath.Join(dir, "objection.json")
	var objection struct {
		Reason   string    `json:"reason"`
		Section  string    `json:"section"`
		RaisedAt time.Time `json:"raised_at"`
	}
	if !readJSON(path, &objection) {
		return
	}
	at := objection.RaisedAt
	if at.IsZero() {
		at = fileTime(path)
	}
	v.Timeline = append(v.Timeline, Event{
		At: at, Step: "objection", Tone: "warn",
		Title:  fmt.Sprintf("実装役の異議 (設計 %d 巡目の %s)", n, objection.Section),
		Why:    shown(objection.Reason),
		Record: fmt.Sprintf("design-%d-objection", n),
	})
}
