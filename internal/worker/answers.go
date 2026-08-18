package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// PreservedAnswer is one settled requester decision from an earlier ticket,
// as preserved in the instance knowledge tree. The readiness pair receives
// these so a settled point is never asked again - a live ticket measurably
// received the same settled question three times before this existed.
type PreservedAnswer struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// The per-file ceiling equals the writer's (MaxAnswerKnowledgeBytes): the
// preserve rail can never produce a record the loader refuses, so a reader
// error here means the tree was edited by hand. The total budget bounds what
// the prompts carry; past it the newest records win and the older ones are
// reported dropped - a growing archive must never stall every ticket's
// readiness (adversarial review measurably reached both cliffs).
const maxPreservedAnswerTotalBytes = 128 * 1024

// LoadPreservedAnswers reads every Markdown answer under the configured
// preservation directory inside knowledgeRoot. No configuration or no
// directory is an empty, valid result; an oversized record is an error,
// because preserve writes small files and anything else means the tree is
// not what this loader believes it is.
func LoadPreservedAnswers(knowledgeRoot string, config Config) ([]PreservedAnswer, int, error) {
	if knowledgeRoot == "" || config.AnswerKnowledge == nil {
		return nil, 0, nil
	}
	directory := filepath.Join(knowledgeRoot, filepath.FromSlash(config.AnswerKnowledge.To))
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, errors.New("preserved answers could not be listed")
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".md") {
			continue
		}
		names = append(names, name)
	}
	// Newest first for the budget below: ticket keys carry ascending
	// numbers, so the descending natural order puts the records most likely
	// to matter ahead of the archive.
	sort.Slice(names, func(i, j int) bool { return answerNameLess(names[j], names[i]) })
	answers := make([]PreservedAnswer, 0, len(names))
	total := 0
	dropped := 0
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, 0, errors.New("preserved answer could not be read: " + name)
		}
		if len(content) > MaxAnswerKnowledgeBytes {
			return nil, 0, errors.New("preserved answer is larger than the preserve rail can write: " + name)
		}
		if total+len(content) > maxPreservedAnswerTotalBytes {
			dropped++
			continue
		}
		total += len(content)
		answers = append(answers, PreservedAnswer{Name: name, Content: string(content)})
	}
	// The prompt reads better oldest-to-newest; the budget above already
	// decided which records survive.
	sort.Slice(answers, func(i, j int) bool { return answerNameLess(answers[i].Name, answers[j].Name) })
	return answers, dropped, nil
}

// answerNameLess orders names with their trailing numbers compared as
// numbers, so TICKET-9 sorts before TICKET-56.
func answerNameLess(a, b string) bool {
	prefixA, numberA, okA := splitTrailingNumber(strings.TrimSuffix(a, ".md"))
	prefixB, numberB, okB := splitTrailingNumber(strings.TrimSuffix(b, ".md"))
	if okA && okB && prefixA == prefixB {
		return numberA < numberB
	}
	return a < b
}

func splitTrailingNumber(value string) (string, int64, bool) {
	index := len(value)
	for index > 0 && value[index-1] >= '0' && value[index-1] <= '9' {
		index--
	}
	if index == len(value) {
		return value, 0, false
	}
	number := int64(0)
	for _, digit := range value[index:] {
		number = number*10 + int64(digit-'0')
		if number > 1<<40 {
			return value, 0, false
		}
	}
	return value[:index], number, true
}

// answersDigestOf seals the exact preserved answers both readiness roles saw,
// mirroring clarificationDigestOf: empty input is the empty digest.
func answersDigestOf(answers []PreservedAnswer) string {
	if len(answers) == 0 {
		return ""
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
