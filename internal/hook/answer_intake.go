package hook

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
)

const (
	// MaxAnswerBodyBytes bounds one answer comment (README「質問、回答、再通知、
	// 再開」: 1,024 byte 超過は採用しない).
	MaxAnswerBodyBytes = 1024
)

// BacklogComment is one observed issue comment from a polling snapshot.
// PostedAt is the Backlog server timestamp in unix milliseconds. The caller
// must supply creation-time content only: an edited body is not an answer
// (README 583), and this layer cannot tell an edit apart on its own.
type BacklogComment struct {
	CommentID int64
	UserID    int64
	Body      string
	PostedAt  int64
}

// AnswerIntakeInput evaluates one polling snapshot against the sealed current
// question. HandledCommentIDs are comments the caller already replied to in
// earlier snapshots. GuidanceSent records that the one-time format guidance
// for this revision has already been posted; the caller must persist it as
// soon as it acts on a returned guidance reply — inferring it from
// HandledCommentIDs alone breaks when the guided comment later drops out of
// the snapshot window.
type AnswerIntakeInput struct {
	Question          QuestionRecord
	QuestionCommentID int64
	AnswererID        int64
	HandledCommentIDs map[int64]bool
	Comments          []BacklogComment
	// Readings is what a model made of each comment in scope, keyed by
	// comment id. The engine does not read a comment itself: whether a
	// person answered, which choice they picked, and whether they are
	// calling the work off are all judgements, and five regular expressions
	// over the first line made them badly enough that a requester who wrote
	// anything but 「回答 C1 Q1:a」 had not answered at all.
	Readings map[int64]AnswerReading
}

// AnswerReading is what a model made of one comment. It mirrors the worker
// package's reading, which is where it is produced; this package may not
// import that one.
type AnswerReading struct {
	Kind       string            `json:"kind"`
	Answers    map[string]string `json:"answers"`
	NotNeeded  []string          `json:"not_needed"`
	Unanswered []string          `json:"unanswered"`
	Reason     string            `json:"reason"`
}

const (
	AnswerReadingAnswer    = "answer"
	AnswerReadingCancel    = "cancel"
	AnswerReadingUnrelated = "unrelated"
)

type AnswerReplyKind string

const (
	AnswerReplyGuidance  AnswerReplyKind = "guidance"
	AnswerReplyShortfall AnswerReplyKind = "shortfall"
)

// AnswerReply asks the caller to respond to one comment: either the one-time
// format guidance, or the immediate shortfall reply re-listing only the
// missing question IDs (in the sealed question order).
type AnswerReply struct {
	CommentID          int64
	Kind               AnswerReplyKind
	MissingQuestionIDs []string
}

// AdoptedAnswerDecision is the single complete valid answer chosen from the
// snapshot, normalized for sealing into a ClarificationRound.
type AdoptedAnswerDecision struct {
	CommentID   int64
	PostedAt    int64
	BodySHA256  string
	AnswersJSON string
}

// CancelDecision is the earliest valid cancellation for the current revision.
type CancelDecision struct {
	CommentID  int64
	PostedAt   int64
	BodySHA256 string
}

// AnswerIntakeDecision is the deterministic outcome for one snapshot. A cancel
// always wins over adoption; when either is set no replies are requested.
type AnswerIntakeDecision struct {
	Cancel  *CancelDecision
	Adopted *AdoptedAnswerDecision
	Replies []AnswerReply
}

type answerQuestion struct {
	id      string
	choices []string
}

// The marker rescue (optional space, lowercase c) keeps a hand-typed near-miss
// from being silently dropped: a requester whose answer gets no reaction at
// all would otherwise wait until the next scheduled renotification, which is
// exactly what the immediate-shortfall contract exists to prevent. Prose that
// merely begins with 回答 (e.g. 回答します) still matches no marker and stays
// ignored.
var (
	answerCandidatePattern = regexp.MustCompile(`^回答[ \t]*[Cc][0-9]+`)
	answerHeaderPattern    = regexp.MustCompile(`^回答[ \t]*[Cc]([0-9]+)$`)
	answerPairPattern      = regexp.MustCompile(`^(?:回答[ \t]*[Cc]([0-9]+)[ \t]+)?[Qq]([0-9]+)[ \t]*:[ \t]*(\S+)$`)
	cancelPattern          = regexp.MustCompile(`^中止[ \t]*[Cc]([0-9]+)$`)
	revisionDigitsPattern  = regexp.MustCompile(`^[0-9]+$`)
)

// EvaluateAnswerIntake decides, for one polling snapshot, whether an answer is
// adopted, the run is cancelled, or replies are owed. The rules follow the
// README contract: only new comments by the allowlisted answerer after the
// question comment and before the sealed deadline count; a comment whose
// first line carries no 回答/中止 revision marker (rescued forms included) is
// ignored; a marker-bearing comment that cannot be interpreted gets the
// format guidance once per revision; a well-formed but incomplete answer gets
// one shortfall reply listing only the missing questions; a first-line cancel
// wins over any answer and the earliest cancel is the evidence; otherwise the
// complete valid answer with the highest comment ID (validated in ascending
// order) is adopted.
func EvaluateAnswerIntake(input AnswerIntakeInput) (AnswerIntakeDecision, error) {
	if err := input.Question.ValidateShape(); err != nil {
		return AnswerIntakeDecision{}, err
	}
	if input.QuestionCommentID <= 0 || input.AnswererID <= 0 {
		return AnswerIntakeDecision{}, errors.New("answer intake binding is invalid")
	}
	comments := append([]BacklogComment{}, input.Comments...)
	sort.Slice(comments, func(left, right int) bool { return comments[left].CommentID < comments[right].CommentID })
	for index := 1; index < len(comments); index++ {
		if comments[index].CommentID == comments[index-1].CommentID {
			return AnswerIntakeDecision{}, errors.New("snapshot comment ids are not unique")
		}
	}

	var cancel *CancelDecision
	var adopted *AdoptedAnswerDecision
	for _, comment := range comments {
		// Which comments are in scope is routing, not reading: the answerer
		// the question was addressed to, posted after the question and
		// before the sealed deadline. Everything later belongs to the expiry
		// transition.
		if comment.UserID != input.AnswererID || comment.CommentID <= input.QuestionCommentID ||
			comment.PostedAt <= 0 || comment.PostedAt >= input.Question.AnswerDeadlineAt {
			continue
		}
		reading, read := input.Readings[comment.CommentID]
		if !read {
			continue
		}
		switch reading.Kind {
		case AnswerReadingCancel:
			// A stop always wins over an answer, and the earliest one is the
			// evidence (README: 起票者による有効な中止コメントが同じ snapshot に
			// 一つでもあれば、回答より中止を優先し、最小 comment ID の中止を
			// 終端証拠にする).
			if cancel == nil {
				cancel = &CancelDecision{
					CommentID:  comment.CommentID,
					PostedAt:   comment.PostedAt,
					BodySHA256: TerminalReportDigest([]byte(comment.Body)),
				}
			}
		case AnswerReadingAnswer:
			if len(reading.Answers) == 0 {
				continue
			}
			encoded, err := json.Marshal(reading.Answers)
			if err != nil || len(encoded) > MaxAnswerSetBytes {
				return AnswerIntakeDecision{}, errors.New("adopted answer set could not be encoded")
			}
			// Ascending, so the last answer the requester wrote is the one
			// adopted. Whether it covers every question is not asked here:
			// the answers go to the role that asked them, and a role that
			// still cannot proceed asks again.
			adopted = &AdoptedAnswerDecision{
				CommentID:   comment.CommentID,
				PostedAt:    comment.PostedAt,
				BodySHA256:  TerminalReportDigest([]byte(comment.Body)),
				AnswersJSON: string(encoded),
			}
		}
	}
	if cancel != nil {
		return AnswerIntakeDecision{Cancel: cancel}, nil
	}
	if adopted != nil {
		return AnswerIntakeDecision{Adopted: adopted}, nil
	}
	// A comment that said nothing about the questions is left alone. The
	// automation used to answer back - a guidance comment telling the person
	// the format they should have used, and a shortfall comment listing what
	// they had missed - and a live delivery stalled for hours because that
	// courtesy could not be posted (2026-09-17).
	return AnswerIntakeDecision{}, nil
}

// decodeIntakeQuestions extracts the question and choice identifiers from the
// sealed questions array. Unknown fields are readiness-owned and ignored here;
// the identifiers themselves must be present, unique and non-empty. The set
// is as long as the reception's one round of questions, up to the protocol
// ceiling — the grammar numbers questions, it does not count them.
func decodeIntakeQuestions(encoded string) ([]answerQuestion, error) {
	var raw []struct {
		ID      string `json:"id"`
		Choices []struct {
			ID string `json:"id"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(encoded), &raw); err != nil {
		return nil, errors.New("question set is invalid")
	}
	if len(raw) < 1 || len(raw) > MaxClarificationQuestions {
		return nil, errors.New("question set is invalid")
	}
	questions := make([]answerQuestion, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		if item.ID == "" || seen[strings.ToLower(item.ID)] {
			return nil, errors.New("question set is invalid")
		}
		seen[strings.ToLower(item.ID)] = true
		if len(item.Choices) < 2 {
			return nil, errors.New("question set is invalid")
		}
		choices := make([]string, 0, len(item.Choices))
		choiceSeen := map[string]bool{}
		for _, choice := range item.Choices {
			if choice.ID == "" || choiceSeen[strings.ToLower(choice.ID)] {
				return nil, errors.New("question set is invalid")
			}
			choiceSeen[strings.ToLower(choice.ID)] = true
			choices = append(choices, choice.ID)
		}
		questions = append(questions, answerQuestion{id: item.ID, choices: choices})
	}
	return questions, nil
}

// normalizeAnswerBody applies the format rescue for hand-typed Japanese input:
// full-width spaces and colons become their ASCII forms. Nothing else is
// rewritten; the sealed digest is always taken over the raw body.
// normalizeAnswerBody rescues what a Japanese keyboard produces when the
// requester is typing an answer rather than prose: a full-width space, a
// full-width colon, and the full-width letters a choice id is written with
// in 全角英数 mode. Without the letters, "ａ" was ignored exactly as the
// bare "a" used to be (review of #192).
func normalizeAnswerBody(body string) string {
	return fullWidthAnswerRunes.Replace(body)
}

var fullWidthAnswerRunes = strings.NewReplacer(
	"　", " ", "：", ":",
	// The letters a choice id is written with (ids are a, b, c, d), the
	// letters the markers use, and the digits of a revision or a question
	// number. Rewriting them is what lets 「回答 Ｃ１ Ｑ１：ａ」 and
	// 「中止Ｃ１」 be read at all.
	"ａ", "a", "ｂ", "b", "ｃ", "c", "ｄ", "d",
	"Ａ", "A", "Ｂ", "B", "Ｃ", "C", "Ｄ", "D",
	"Ｑ", "Q", "ｑ", "q",
	"０", "0", "１", "1", "２", "2", "３", "3", "４", "4",
	"５", "5", "６", "6", "７", "7", "８", "8", "９", "9",
)

func firstContentLine(body string) string {
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func revisionMarker(revision int) string {
	digits := []byte{}
	for value := revision; value > 0; value /= 10 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
	}
	return string(digits)
}

// parseAnswerBody interprets one candidate comment. It returns the parsed
// answers keyed by canonical question ID, the ascending list of question IDs
// still unanswered, and whether the comment stayed inside the grammar. The
// copy-paste line form (`回答 C1 Q1:a` repeated per question) and the block
// form (header line then `Q1: a` lines) are both accepted, in any line order:
// the README rejection set (unknown question, unknown choice, duplicate,
// omission, extra prose, oversize) deliberately does not include ordering, so
// pasted lines are not punished for it.
func parseAnswerBody(body string, revision int, questions []answerQuestion) (map[string]string, []string, bool) {
	marker := revisionMarker(revision)
	lines := contentLines(body)
	if len(lines) == 0 {
		return nil, nil, false
	}
	// One question, one line, and that line names one of its choices: the
	// answer is the choice. Requiring "回答 C1 Q1:a" from a person facing a
	// single question with two options is a format demand, not a question -
	// a requester answered "a" and was ignored, and the run waited on
	// (live 2026-09-17: 「質問文は 回答 C1 Q1:a、馬鹿だな、解釈しろし」).
	// Only the current question's own choices are accepted, and only for a
	// comment posted after that question, so a bare word can never be read
	// as an answer to a question it was not shown.
	if len(questions) == 1 && len(lines) == 1 {
		if choice, ok := soleChoice(questions[0], lines[0]); ok {
			return map[string]string{questions[0].id: choice}, []string{}, true
		}
	}
	answers := map[string]string{}
	sawHeader := false
	for index, line := range lines {
		if header := answerHeaderPattern.FindStringSubmatch(line); header != nil {
			// A bare header is only the opening line; repeated headers are
			// extra prose.
			if index != 0 || header[1] != marker {
				return nil, nil, false
			}
			sawHeader = true
			continue
		}
		pair := answerPairPattern.FindStringSubmatch(line)
		if pair == nil {
			return nil, nil, false
		}
		inlineMarker := pair[1]
		if inlineMarker == "" {
			// A bare `Qn: x` line belongs to the block form under a header.
			if !sawHeader {
				return nil, nil, false
			}
		} else if inlineMarker != marker {
			return nil, nil, false
		} else if index == 0 {
			sawHeader = true
		}
		question, choice, ok := resolveAnswerPair(questions, pair[2], pair[3])
		if !ok {
			return nil, nil, false
		}
		if _, duplicate := answers[question]; duplicate {
			return nil, nil, false
		}
		answers[question] = choice
	}
	missing := []string{}
	for _, question := range questions {
		if _, answered := answers[question.id]; !answered {
			missing = append(missing, question.id)
		}
	}
	return answers, missing, true
}

// resolveAnswerPair matches the typed question number and choice token against
// the sealed identifiers, case-insensitively.
func resolveAnswerPair(questions []answerQuestion, number, choiceToken string) (string, string, bool) {
	if !revisionDigitsPattern.MatchString(number) {
		return "", "", false
	}
	typed := "q" + strings.TrimLeft(number, "0")
	if typed == "q" {
		return "", "", false
	}
	for _, question := range questions {
		if strings.ToLower(question.id) != typed {
			continue
		}
		for _, choice := range question.choices {
			if strings.EqualFold(choice, choiceToken) {
				return question.id, choice, true
			}
		}
		return "", "", false
	}
	return "", "", false
}

// soleChoice reads a line that names nothing but one of the question's
// choices: "a", "A", "a." or "a。" - the shapes a person types when there is
// one question in front of them. Anything else is not an answer here.
func soleChoice(question answerQuestion, line string) (string, bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(line), ".。、,)）")
	trimmed = strings.TrimSpace(strings.TrimLeft(trimmed, "(（"))
	if trimmed == "" {
		return "", false
	}
	for _, choice := range question.choices {
		if strings.EqualFold(trimmed, choice) {
			return choice, true
		}
	}
	return "", false
}

// bareChoiceAnswer reports whether the whole comment is one of the sole
// question's choices.
func bareChoiceAnswer(questions []answerQuestion, body string) bool {
	if len(questions) != 1 {
		return false
	}
	lines := contentLines(body)
	if len(lines) != 1 {
		return false
	}
	_, ok := soleChoice(questions[0], lines[0])
	return ok
}

// contentLines is the comment's non-empty lines, trimmed.
func contentLines(body string) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}
