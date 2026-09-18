package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// The comment bodies below are deterministic functions of the sealed question
// record (plus explicit arguments), never of the wall clock. Backlog's comment
// API has no idempotency key, so a lost response is repaired by exact-content
// lookup — determinism is what makes that repair sound. The shortfall body
// embeds the comment it replies to so two shortfalls never collide.

type questionMessageChoice struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
}

type questionMessageItem struct {
	ID          string                  `json:"id"`
	Question    string                  `json:"question"`
	WhyBlocking string                  `json:"why_blocking"`
	Choices     []questionMessageChoice `json:"choices"`
}

func decodeQuestionMessageItems(encoded string) ([]questionMessageItem, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	var items []questionMessageItem
	if err := decoder.Decode(&items); err != nil {
		return nil, errors.New("question set is invalid")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("question set is invalid")
	}
	if len(items) < 1 || len(items) > MaxClarificationQuestions {
		return nil, errors.New("question set is invalid")
	}
	for _, item := range items {
		if item.ID == "" || item.Question == "" || len(item.Choices) < 2 {
			return nil, errors.New("question set is invalid")
		}
		for _, choice := range item.Choices {
			if choice.ID == "" || choice.Label == "" {
				return nil, errors.New("question set is invalid")
			}
		}
	}
	return items, nil
}

func formatQuestionInstant(unixMilli int64) string {
	return time.UnixMilli(unixMilli).In(questionZone).Format("2006-01-02 15:04")
}

// QuestionRevisionTag is the number a requester types after 回答 or 中止:
// the round they are being asked about. A board that names a round the run
// is not on sends the requester to a comment nobody reads.
func QuestionRevisionTag(revision int) string {
	return fmt.Sprintf("C%d", revision)
}

// QuestionCommentContent renders the clarification questions with one
// copy-paste answer line under every choice, so the requester answers with a
// single paste instead of typing (README 584).
func QuestionCommentContent(record QuestionRecord) (string, error) {
	if err := record.ValidateShape(); err != nil {
		return "", err
	}
	items, err := decodeQuestionMessageItems(record.QuestionsJSON)
	if err != nil {
		return "", err
	}
	tag := QuestionRevisionTag(record.QuestionRevision)
	var builder strings.Builder
	fmt.Fprintf(&builder, "【確認のお願い %s】回答期限: %s\n\n", tag, formatQuestionInstant(record.AnswerDeadlineAt))
	builder.WriteString("このチケットの自動処理を進めるために、以下の確認が必要です。対象リポジトリと本番環境には、まだ何も変更を加えていません。\n")
	builder.WriteString("回答は、選びたい選択肢の下にある「回答 " + tag + " ...」の行を、そのままコメントに貼り付けて投稿してください。質問が複数ある場合は、各質問の行を 1 つのコメントにまとめてください。\n")
	if len(items) == 1 && len(items[0].Choices) > 0 {
		builder.WriteString("この質問は 1 問だけなので、選択肢の記号 (例: " + items[0].Choices[0].ID + ") だけをコメントしても受け付けます。\n")
	}
	for _, item := range items {
		fmt.Fprintf(&builder, "\n%s. %s\n", item.ID, item.Question)
		if item.WhyBlocking != "" {
			fmt.Fprintf(&builder, "(確認する理由: %s)\n", item.WhyBlocking)
		}
		for _, choice := range item.Choices {
			fmt.Fprintf(&builder, "- %s: %s", choice.ID, choice.Label)
			if choice.Effect != "" {
				fmt.Fprintf(&builder, " — %s", choice.Effect)
			}
			builder.WriteString("\n")
			fmt.Fprintf(&builder, "  回答 %s %s:%s\n", tag, item.ID, choice.ID)
		}
	}
	fmt.Fprintf(&builder, "\n処理を中止する場合は「中止 %s」とだけコメントしてください。\n", tag)
	builder.WriteString(CommentFacts{
		State:      "回答待ち（質問 " + tag + "）",
		NextActor:  "起票者（回答者）",
		Operation:  "選びたい選択肢の下の「回答 " + tag + " ...」行を、1 つのコメントに貼り付けて投稿",
		NextEvent:  "次回通知 " + formatQuestionInstant(record.NotifyAt[0]) + " / 回答期限 " + formatQuestionInstant(record.AnswerDeadlineAt) + "（期限を過ぎると変更を加えずに停止）",
		Production: "未変更",
		AutoRetry:  "なし（回答があれば自動で再開）",
		Marker:     CommentMarker("question", record.AutomationRunID, tag),
	}.render())
	return builder.String(), nil
}

// NotifyCommentContent is scheduled reminder number index (1..3).
func NotifyCommentContent(record QuestionRecord, index int) (string, error) {
	if err := record.ValidateShape(); err != nil {
		return "", err
	}
	if index < 1 || index > QuestionNotifyCount {
		return "", errors.New("notify index is invalid")
	}
	tag := QuestionRevisionTag(record.QuestionRevision)
	var builder strings.Builder
	fmt.Fprintf(&builder, "【再通知 %d/%d %s】回答期限: %s\n\n", index, QuestionNotifyCount, tag, formatQuestionInstant(record.AnswerDeadlineAt))
	builder.WriteString("確認事項への回答をお待ちしています。質問コメントの選択肢の下にある「回答 " + tag + " ...」の行を、そのままコメントに貼り付けて投稿してください。\n")
	fmt.Fprintf(&builder, "処理を中止する場合は「中止 %s」とだけコメントしてください。\n", tag)
	nextEvent := "回答期限 " + formatQuestionInstant(record.AnswerDeadlineAt) + "（以後の再通知はありません。期限を過ぎると変更を加えずに停止）"
	if index < QuestionNotifyCount {
		nextEvent = "次回通知 " + formatQuestionInstant(record.NotifyAt[index]) + " / 回答期限 " + formatQuestionInstant(record.AnswerDeadlineAt) + "（期限を過ぎると変更を加えずに停止）"
	}
	builder.WriteString(CommentFacts{
		State:      fmt.Sprintf("回答待ち（再通知 %d/%d・質問 %s）", index, QuestionNotifyCount, tag),
		NextActor:  "起票者（回答者）",
		Operation:  "質問コメントの「回答 " + tag + " ...」行を、そのまま貼り付けて投稿",
		NextEvent:  nextEvent,
		Production: "未変更",
		AutoRetry:  "なし（回答があれば自動で再開）",
		Marker:     CommentMarker("renotify", record.AutomationRunID, tag, strconv.Itoa(index)),
	}.render())
	return builder.String(), nil
}
