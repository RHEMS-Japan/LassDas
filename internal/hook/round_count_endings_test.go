package hook

import (
	"strings"
	"testing"
)

// An ending that comes from a round count names the count that ended it.
//
// These three sentences outlived the contract that produced them: a
// destination declaring how many rounds it would pay for, the third of
// which ended the delivery. Rounds are not counted out by default now, so
// a requester told the reviews 「最大回数内に収束しなかった」 would go looking
// for the number that stopped their ticket and find that nothing was set.
// What actually ends these deliveries is the highest round number any
// record can carry, or a number an operator chose.
func TestARoundCountEndingNamesTheCountThatEndedIt(t *testing.T) {
	for _, c := range []struct {
		code  TerminalCode
		says  []string
		stale []string
	}{
		{
			code:  TerminalNonconverged,
			says:  []string{"記録の上限（50 巡）", "運用担当者が設定した巡数"},
			stale: []string{"最大回数"},
		},
		{
			code:  TerminalInvestigationNonconverged,
			says:  []string{"記録の上限（50 巡）"},
			stale: []string{"規定回数"},
		},
		{
			code:  TerminalDesignNonconverged,
			says:  []string{"記録の上限（50 巡）"},
			stale: []string{"規定回数"},
		},
		{
			// This one says a budget ran out, which is true of the ceiling
			// and stays as it is (three other tests pin its wording). What
			// it must not start doing is point at a prescribed number, the
			// way the three above did.
			code:  TerminalDesignRoundsSpent,
			stale: []string{"規定回数", "最大回数"},
		},
	} {
		t.Run(string(c.code), func(t *testing.T) {
			report := terminalTestRequest(c.code)
			comment := TerminalCommentContent(report, strings.Repeat("a", 64))
			for _, said := range c.says {
				if !strings.Contains(comment, said) {
					t.Errorf("the ending does not say %q:\n%s", said, comment)
				}
			}
			for _, stale := range c.stale {
				if strings.Contains(comment, stale) {
					t.Errorf("the ending still speaks of %q, which no default sets:\n%s", stale, comment)
				}
			}
		})
	}
}
