package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// judgeFunc scripts the judgement for a test.
type judgeFunc func(state string) (bool, string, error)

func (f judgeFunc) StillProgressing(_ context.Context, state string) (bool, string, error) {
	return f(state)
}

func watchFast(judge ProgressJudge, strikes int) ProgressWatch {
	return ProgressWatch{Judge: judge, Every: 5 * time.Millisecond, Grace: time.Millisecond,
		Strikes: strikes, MaxStateBytes: 1024}
}

// A run that has stopped getting anywhere is ended, and the reason travels
// with it. Live this ran for 60 minutes into a failure, and for 458 of 500
// iterations repeating the same search.
func TestAStalledRunIsEndedWithTheReason(t *testing.T) {
	output := &lockedTranscript{}
	_, _ = output.Write([]byte("searching for ExtractArchive\nsearching for ExtractArchive\n"))
	var stopped *StalledError
	done := make(chan struct{})
	watch := watchFast(judgeFunc(func(string) (bool, string, error) {
		return false, "same searches repeated without changing anything", nil
	}), 3)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		watch.Watch(ctx, output, func(e *StalledError) { stopped = e; close(done) })
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("止まった走行が終わらされませんでした")
	}
	if stopped == nil || !strings.Contains(stopped.Error(), "same searches") {
		t.Fatalf("理由が伝わっていません: %+v", stopped)
	}
	if stopped.Looks < 3 {
		t.Fatalf("%d 回の観測で終わらせました (3 回連続のはず)", stopped.Looks)
	}
}

// 1 回の観測は状態ではない。進んでいる合図が 1 つ入れば、数え直す。
func TestOneBadLookDoesNotEndARun(t *testing.T) {
	output := &lockedTranscript{}
	_, _ = output.Write([]byte("working"))
	looks := 0
	var mu sync.Mutex
	watch := watchFast(judgeFunc(func(string) (bool, string, error) {
		mu.Lock()
		defer mu.Unlock()
		looks++
		// 止まっている・進んでいる・止まっている… と交互に見える走行。
		return looks%2 == 0, "", nil
	}), 3)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	ended := false
	watch.Watch(ctx, output, func(*StalledError) { ended = true })
	if ended {
		t.Fatal("進んでいる合図があるのに終わらされました")
	}
}

// 判定が得られないときは殺さない。見えない見張りが人の作業を止めるのは、
// 見張りが壊れたときに全依頼が死ぬということ。
func TestAWatchThatCannotSeeDoesNotKill(t *testing.T) {
	output := &lockedTranscript{}
	_, _ = output.Write([]byte("working"))
	watch := watchFast(judgeFunc(func(string) (bool, string, error) {
		return false, "", errors.New("decision gateway unreachable")
	}), 2)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	ended := false
	watch.Watch(ctx, output, func(*StalledError) { ended = true })
	if ended {
		t.Fatal("判定が取れないのに走行を終わらせました")
	}
}

// 出力がまだ 1 文字も無い走行は、止まっているのではなく始まっていない。
func TestASilentRunIsNotJudgedStalled(t *testing.T) {
	output := &lockedTranscript{}
	asked := false
	watch := watchFast(judgeFunc(func(string) (bool, string, error) {
		asked = true
		return false, "", nil
	}), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ended := false
	watch.Watch(ctx, output, func(*StalledError) { ended = true })
	if asked || ended {
		t.Fatalf("無音の走行を判定しました (asked=%v ended=%v)", asked, ended)
	}
}

// 見張りを付けない走行は、これまで通り動く。既定で全員に効く変更ではない。
func TestNoJudgeMeansNoWatch(t *testing.T) {
	output := &lockedTranscript{}
	_, _ = output.Write([]byte("working"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ended := false
	ProgressWatch{Every: time.Millisecond, Grace: time.Millisecond}.Watch(ctx, output, func(*StalledError) { ended = true })
	if ended {
		t.Fatal("見張りが無いのに終わらされました")
	}
}

// 判定モデルには 2 つの問いが 1 往復で飛び、両方が揃ったときだけ「止まって
// いる」になる。探索を繰り返しながら編集している走行は働いている。
func TestBothSignalsMustHoldBeforeARunCountsAsStalled(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = io.WriteString(w, `{"repeating":{"type":"noul","noul":0.95},"changing_nothing":{"type":"noul","noul":0.2}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	judge := DecisionProgressJudge{Client: client, Endpoint: decisionEndpoint(server.URL)}

	progressing, _, err := judge.StillProgressing(context.Background(), "searching... editing main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !progressing {
		t.Fatal("編集を続けている走行を止まっていると判定しました")
	}
	for _, want := range []string{stalledRepeatQuestion, stalledChangeQuestion} {
		if !strings.Contains(body, want) {
			t.Errorf("問い %q が送られていません", want)
		}
	}
	if strings.Count(body, `"noul"`) != 2 {
		t.Errorf("1 往復に 2 問が載っていません: %s", body)
	}
}

// 確信が閾値に届かない観測は、止まっている証拠として数えない。
func TestAnUnsureLookDoesNotCountAgainstTheRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"repeating":{"type":"noul","noul":0.6},"changing_nothing":{"type":"noul","noul":0.6}}`)
	}))
	defer server.Close()
	t.Setenv("DECISION_KEY", "k")
	client, _ := NewSystemOneClient(server.Client())
	judge := DecisionProgressJudge{Client: client, Endpoint: decisionEndpoint(server.URL), Threshold: 0.8}
	progressing, _, err := judge.StillProgressing(context.Background(), "hmm")
	if err != nil || !progressing {
		t.Fatalf("progressing=%v err=%v", progressing, err)
	}
}
