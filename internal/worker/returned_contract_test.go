package worker

import (
	"strings"
	"testing"
	"time"
)

// A returned report is not authority to change the original contract. These
// reports quote explicit constraints rather than asking the engine to choose
// an unspecified implementation detail.
func TestAReturnDoesNotAuthorizeChangingTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name, report, forbidden string
	}{
		{
			"an explicit no-change condition",
			"The requested reference is absent. The original request says not to infer its contents and to make no changes if it is unavailable. I made no changes.",
			"変更を 1 つも加えずに終了することはできません。",
		},
		{
			"a required real integration",
			"The API key is unavailable. The request explicitly requires the real integration and prohibits a fake as the delivered behavior. I made no changes.",
			"本物を使わずに代役 (test double / fake) を実装し、決められた検証が通る状態にしてください。",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, err := SealAgentRun(returnedRun(tc.report))
			if err != nil {
				t.Fatal(err)
			}
			answer := AnswerReturn(run, nil, time.Now().UTC())
			if strings.Contains(answer.Instruction, tc.forbidden) {
				t.Fatalf("recovery contradicts the original constraint: %s", tc.forbidden)
			}
			for _, required := range []string{
				"元の依頼・設計・変更範囲・検収条件・納品先を変更する許可ではありません",
				"未指定の実装詳細に限り",
				"変更禁止の条件を守ってください",
				"変更が無いというだけで成功にはなりません",
				"要求された実接続や本番納品の代わりにしてはいけません",
				"権限・予算を増やしたり",
				"運用担当者が後で用意すると約束したりしない",
			} {
				if !strings.Contains(answer.Instruction, required) {
					t.Errorf("recovery lost the boundary %q", required)
				}
			}
			if answer.Assumption.Kind != AssumptionImplementerReturn ||
				!strings.Contains(answer.Assumption.Statement, "確認したわけではない") {
				t.Fatalf("the return is reported as an unverified resolution: %+v", answer.Assumption)
			}
		})
	}
}

func TestOldReturnInstructionsAreHistoryNotCurrentAuthority(t *testing.T) {
	r := AnswerReturn(launch(t, "API key unavailable."), nil, answeredAt())
	r.Instruction = "Weaken the acceptance criteria and deliver a fake."
	before := r
	prompt := r.ContinuationInstruction()
	if strings.Contains(prompt, r.Instruction) || !strings.Contains(prompt, "元の条件を優先") {
		t.Fatalf("the old instruction was reissued: %s", prompt)
	}
	if r.Instruction != before.Instruction || r.ReportSHA256 != before.ReportSHA256 {
		t.Fatal("rendering overwrote the historical evidence")
	}
	r.Answered = false
	if r.ContinuationInstruction() != "" {
		t.Fatal("a return left to the ladder was answered during rendering")
	}
}
