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
	GuidanceSent      bool
	HandledCommentIDs map[int64]bool
	Comments          []BacklogComment
}

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
	questions, err := decodeIntakeQuestions(input.Question.QuestionsJSON)
	if err != nil {
		return AnswerIntakeDecision{}, err
	}
	comments := append([]BacklogComment{}, input.Comments...)
	sort.Slice(comments, func(left, right int) bool { return comments[left].CommentID < comments[right].CommentID })
	for index := 1; index < len(comments); index++ {
		if comments[index].CommentID == comments[index-1].CommentID {
			return AnswerIntakeDecision{}, errors.New("snapshot comment ids are not unique")
		}
	}

	type evaluated struct {
		comment BacklogComment
		answers map[string]string
		missing []string
		invalid bool
	}
	var cancel *CancelDecision
	var candidates []evaluated
	for _, comment := range comments {
		// Only new comments by the fixed answerer, posted after the question
		// and before the sealed deadline, take part. Everything later belongs
		// to the expiry transition.
		if comment.UserID != input.AnswererID || comment.CommentID <= input.QuestionCommentID ||
			comment.PostedAt <= 0 || comment.PostedAt >= input.Question.AnswerDeadlineAt {
			continue
		}
		body := normalizeAnswerBody(comment.Body)
		// A stop expressed on the first line is a cancel even when politeness
		// follows on later lines; converting an expressed stop into a resume
		// would be the worse failure. A 中止 line buried below an answer body
		// stays a non-cancel.
		if match := cancelPattern.FindStringSubmatch(firstContentLine(body)); match != nil {
			if match[1] == revisionMarker(input.Question.QuestionRevision) && cancel == nil {
				cancel = &CancelDecision{
					CommentID:  comment.CommentID,
					PostedAt:   comment.PostedAt,
					BodySHA256: TerminalReportDigest([]byte(comment.Body)),
				}
			}
			continue
		}
		if !answerCandidatePattern.MatchString(strings.TrimSpace(body)) {
			continue
		}
		answers, missing, ok := parseAnswerBody(body, input.Question.QuestionRevision, questions)
		if len(comment.Body) > MaxAnswerBodyBytes {
			ok = false
		}
		candidates = append(candidates, evaluated{comment: comment, answers: answers, missing: missing, invalid: !ok})
	}
	// The requester's explicit stop always wins over answers (README: 許可
	// 起票者による有効な中止コメントが同じ snapshot に一つでもあれば、回答より
	// 中止を優先し、最小 comment ID の中止を終端証拠にする).
	if cancel != nil {
		return AnswerIntakeDecision{Cancel: cancel}, nil
	}
	var adopted *AdoptedAnswerDecision
	for _, candidate := range candidates {
		if candidate.invalid || len(candidate.missing) > 0 {
			continue
		}
		encoded, err := json.Marshal(candidate.answers)
		if err != nil || len(encoded) > MaxAnswerSetBytes {
			return AnswerIntakeDecision{}, errors.New("adopted answer set could not be encoded")
		}
		// Ascending validation ends with the highest complete valid comment
		// adopted.
		adopted = &AdoptedAnswerDecision{
			CommentID:   candidate.comment.CommentID,
			PostedAt:    candidate.comment.PostedAt,
			BodySHA256:  TerminalReportDigest([]byte(candidate.comment.Body)),
			AnswersJSON: string(encoded),
		}
	}
	if adopted != nil {
		return AnswerIntakeDecision{Adopted: adopted}, nil
	}
	decision := AnswerIntakeDecision{}
	guidanceSent := input.GuidanceSent
	// An already-handled invalid comment means the one-time guidance went out
	// in an earlier snapshot; settle that before deciding on new replies.
	for _, candidate := range candidates {
		if candidate.invalid && input.HandledCommentIDs[candidate.comment.CommentID] {
			guidanceSent = true
		}
	}
	for _, candidate := range candidates {
		if input.HandledCommentIDs[candidate.comment.CommentID] {
			continue
		}
		if candidate.invalid {
			if guidanceSent {
				continue
			}
			guidanceSent = true
			decision.Replies = append(decision.Replies, AnswerReply{CommentID: candidate.comment.CommentID, Kind: AnswerReplyGuidance})
			continue
		}
		decision.Replies = append(decision.Replies, AnswerReply{
			CommentID: candidate.comment.CommentID, Kind: AnswerReplyShortfall, MissingQuestionIDs: candidate.missing,
		})
	}
	return decision, nil
}

// decodeIntakeQuestions extracts the question and choice identifiers from the
// sealed questions array. Unknown fields are readiness-owned and ignored here;
// the identifiers themselves must be present, unique and non-empty.
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
func normalizeAnswerBody(body string) string {
	return strings.NewReplacer("　", " ", "：", ":").Replace(body)
}

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
	lines := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return nil, nil, false
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
