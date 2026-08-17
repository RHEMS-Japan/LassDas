package worker

import (
	"errors"
	"fmt"
	"strings"
)

// MaxAnswerKnowledgeBytes bounds one rendered answer record. The sealed
// clarification record it renders from is itself bounded, so hitting this
// means the render is broken, not that the requester wrote too much.
const MaxAnswerKnowledgeBytes = 256 * 1024

// AnswerKnowledgeConfig names where adopted answers are written back so the
// next run reads them instead of asking again. Like everything an agent is
// given to read, this is configuration, not a channel: an instance that does
// not name a place gets nothing written, and the place is expected to sit
// inside the knowledge tree the agents already read.
type AnswerKnowledgeConfig struct {
	To string `json:"to"`
}

func (a AnswerKnowledgeConfig) validate() error {
	if !validRelativePath(a.To) || hasHiddenComponent(a.To) {
		return errors.New("answer knowledge destination is invalid")
	}
	return nil
}

// AnswerKnowledgeArtifact is what one resumed run leaves behind: the rendered
// record and the repository path it belongs at. Enabled is false when the
// instance has not named a destination, so the caller can tell "nothing to
// write" apart from a failed render.
type AnswerKnowledgeArtifact struct {
	Enabled  bool   `json:"enabled"`
	Path     string `json:"path,omitempty"`
	IssueKey string `json:"issue_key,omitempty"`
	Content  []byte `json:"-"`
}

// PreserveAnswerKnowledge renders the adopted answers of a resumed run into
// the deterministic record the instance keeps. The clarification must belong
// to the same sealed input as the ticket: a record from another delivery must
// never be written under this ticket's name.
func PreserveAnswerKnowledge(config Config, raw RawTicket, clarification *ClarificationContext) (AnswerKnowledgeArtifact, error) {
	if err := config.Validate(); err != nil {
		return AnswerKnowledgeArtifact{}, errors.New("worker configuration is invalid")
	}
	if err := raw.Validate(config); err != nil {
		return AnswerKnowledgeArtifact{}, err
	}
	if clarification == nil || len(clarification.Exchanges) == 0 {
		return AnswerKnowledgeArtifact{}, errors.New("clarification record is missing")
	}
	if clarification.DeliveryID != raw.DeliveryID || clarification.InputSHA256 != raw.InputSHA256 {
		return AnswerKnowledgeArtifact{}, errors.New("clarification does not belong to this ticket")
	}
	if config.AnswerKnowledge == nil {
		return AnswerKnowledgeArtifact{Enabled: false}, nil
	}
	content, err := renderAnswerKnowledge(raw, clarification)
	if err != nil {
		return AnswerKnowledgeArtifact{}, err
	}
	return AnswerKnowledgeArtifact{
		Enabled:  true,
		Path:     config.AnswerKnowledge.To + "/" + raw.IssueKey + ".md",
		IssueKey: raw.IssueKey,
		Content:  content,
	}, nil
}

// renderAnswerKnowledge lays the exchanges out without a model in the loop.
// The questions and answers are already structured and validated; a
// deterministic render adds nothing that was not decided, so the record can
// never say more than the requester did.
func renderAnswerKnowledge(raw RawTicket, clarification *ClarificationContext) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# 起票者の回答記録: %s\n\n", raw.IssueKey)
	b.WriteString("自動処理が確認事項を質問し、起票者が回答した記録。同じ論点を再び判断するときは、新しい指示がない限りこの回答に従う。\n\n")
	fmt.Fprintf(&b, "- チケット: %s — %s\n", raw.IssueKey, answerInline(raw.Summary))
	fmt.Fprintf(&b, "- 回答 revision: %d\n", clarification.Revision)
	fmt.Fprintf(&b, "- 記録 digest: sha256:%s\n", clarification.SHA256)
	for round, exchange := range clarification.Exchanges {
		for _, question := range exchange.Questions {
			fmt.Fprintf(&b, "\n## 確認 %d-%s: %s\n\n", round+1, question.ID, answerInline(question.Question))
			if question.WhyBlocking != "" {
				fmt.Fprintf(&b, "- なぜ確認したか: %s\n", answerInline(question.WhyBlocking))
			}
			for _, choice := range question.Choices {
				fmt.Fprintf(&b, "- 選択肢 %s: %s — %s\n", answerInline(choice.ID), answerInline(choice.Label), answerInline(choice.Effect))
			}
			answer, answered := exchange.Answers[question.ID]
			if !answered {
				b.WriteString("- **回答: (記録なし)**\n")
				continue
			}
			if chosen, found := choiceByID(question.Choices, answer); found {
				fmt.Fprintf(&b, "- **回答: %s — %s**\n", answerInline(chosen.ID), answerInline(chosen.Label))
				fmt.Fprintf(&b, "- 採用された結果: %s\n", answerInline(chosen.Effect))
				continue
			}
			fmt.Fprintf(&b, "- **回答: %s**\n", answerInline(answer))
		}
	}
	if b.Len() > MaxAnswerKnowledgeBytes {
		return nil, errors.New("answer knowledge render is larger than allowed")
	}
	return []byte(b.String()), nil
}

func choiceByID(choices []ReadinessChoice, answer string) (ReadinessChoice, bool) {
	for _, choice := range choices {
		if choice.ID == answer {
			return choice, true
		}
	}
	return ReadinessChoice{}, false
}

// answerInline flattens one recorded string onto a single line and drops
// control characters, so a value can never break out of the list item that
// quotes it.
func answerInline(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	var b strings.Builder
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
