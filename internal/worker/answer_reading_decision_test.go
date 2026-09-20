package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sealedQuestions = `[{"id":"Q1","question":"空のリストをどう扱いますか",
  "choices":[{"id":"a","label":"拒否する","effect":"保存されません"},
             {"id":"b","label":"解除と同じ","effect":"全部許可になります"}]},
 {"id":"Q2","question":"既存の値に遡りますか",
  "choices":[{"id":"a","label":"遡る"},{"id":"b","label":"新規のみ"}]}]`

// decisionServer answers with whatever the test scripts, and records the
// request so the shape sent can be checked.
func decisionServer(t *testing.T, reply string, seen *SystemOneRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if seen != nil {
			_ = json.Unmarshal(raw, seen)
		}
		_, _ = io.WriteString(w, reply)
	}))
}

// The reading a decision model returns is the same reading the prose path
// returns: what the comment is, and which choice it picks per question.
func TestADecisionReadsOneCommentIntoTheSameShape(t *testing.T) {
	var sent SystemOneRequest
	server := decisionServer(t, `{"__kind":{"type":"choice","choice":"answer"},
		"Q1":{"type":"choice","choice":"a"},
		"Q2":{"type":"choice","choice":"__not_answered"}}`, &sent)
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())

	reading, err := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), sealedQuestions, "a でお願いします")
	if err != nil {
		t.Fatal(err)
	}
	if reading.Kind != AnswerReadingAnswer {
		t.Fatalf("kind = %q", reading.Kind)
	}
	if reading.Answers["Q1"] != "a" {
		t.Fatalf("answers = %v", reading.Answers)
	}
	if _, picked := reading.Answers["Q2"]; picked {
		t.Fatalf("答えていない質問に答えが入りました: %v", reading.Answers)
	}
	if len(reading.Unanswered) != 1 || reading.Unanswered[0] != "Q2" {
		t.Fatalf("unanswered = %v", reading.Unanswered)
	}
	if !reading.IsAnswer() {
		t.Fatal("採用の判定が通りません")
	}

	// One round trip carries every question, plus the one that asks what
	// the comment is, and the comment itself is the state.
	if sent.State != "a でお願いします" {
		t.Errorf("state = %q", sent.State)
	}
	if len(sent.Questions) != 3 {
		t.Fatalf("送った質問 = %d 件", len(sent.Questions))
	}
	// Every question offers a way to say "this one is not answered here",
	// or a comment answering two of three would invent the third.
	for id, q := range sent.Questions {
		if id == decisionKindQuestion {
			continue
		}
		options, ok := q.Criteria.(map[string]any)
		if !ok {
			t.Fatalf("%s の選択肢の形 = %T", id, q.Criteria)
		}
		if _, ok := options[decisionNotAnswered]; !ok {
			t.Errorf("%s に「答えていない」が無い: %v", id, options)
		}
	}
}

// 取り消しは回答より優先される規則が上にあるので、読み取りは取り消しを
// 取り消しとして返すだけでよい。
func TestAStopIsReadAsAStop(t *testing.T) {
	server := decisionServer(t, `{"__kind":{"type":"choice","choice":"cancel"},
		"Q1":{"type":"choice","choice":"__not_answered"},
		"Q2":{"type":"choice","choice":"__not_answered"}}`, nil)
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	reading, err := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), sealedQuestions, "この依頼は取り消します")
	if err != nil {
		t.Fatal(err)
	}
	if reading.Kind != AnswerReadingCancel || reading.IsAnswer() {
		t.Fatalf("reading = %+v", reading)
	}
}

// 「答えだ」と言いながら 1 つも選んでいない読みは答えではない。採用側が
// 少なくとも 1 つを求めるので、ここで形を揃えておく。
func TestAnAnswerThatPickedNothingIsNotAnAnswer(t *testing.T) {
	server := decisionServer(t, `{"__kind":{"type":"choice","choice":"answer"},
		"Q1":{"type":"choice","choice":"__not_answered"},
		"Q2":{"type":"choice","choice":"__not_answered"}}`, nil)
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	reading, _ := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), sealedQuestions, "うーん")
	if reading.IsAnswer() || reading.Kind == AnswerReadingAnswer {
		t.Fatalf("reading = %+v", reading)
	}
}

// 封緘された質問が読めないなら、読み取りは成立しない。言い換えた質問に
// 対して答えを作ると、誰も聞いていない問いに答えたことになる。
func TestAReadingWithoutTheSealedQuestionsIsRefused(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	for name, questions := range map[string]string{
		"空":           "",
		"JSON ではない":   "{{{",
		"0 件":         "[]",
		"id が無い":      `[{"question":"q","choices":[{"id":"a","label":"l"}]}]`,
		"選択肢が無い":      `[{"id":"Q1","question":"q","choices":[]}]`,
		"予約した id と衝突": `[{"id":"__kind","question":"q","choices":[{"id":"a","label":"l"}]}]`,
	} {
		if _, err := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), questions, "a"); err == nil {
			t.Errorf("%s が受け入れられました", name)
		}
	}
	if sent {
		t.Error("読めない質問のまま gateway へ送られました")
	}
}

// 空のコメントは何も言っていない。以前ここで毎分の再試行が起きている。
func TestAnEmptyCommentIsNotSent(t *testing.T) {
	sent := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sent = true }))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	if _, err := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), sealedQuestions, "   "); err == nil {
		t.Error("空のコメントが送られました")
	}
	if sent {
		t.Error("空のコメントが gateway へ送られました")
	}
}

// 失敗は握り潰さない。呼び出し元が文章の経路へ戻すかどうかを決められる
// ように、理由をそのまま返す。
func TestAFailedDecisionIsReturnedNotSwallowed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"Model not allowed for this key"}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	_, err := client.ReadAnswerByDecision(context.Background(), decisionEndpoint(server.URL), sealedQuestions, "a")
	if err == nil || !strings.Contains(err.Error(), "Model not allowed") {
		t.Fatalf("理由が返っていません: %v", err)
	}
}
