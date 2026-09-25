package runtime

import (
	"testing"
	"time"
)

func TestOperatorAllowedIsTheRequesterOrAListedOperator(t *testing.T) {
	tracker := TrackerConfig{AllowedCreatorID: 7001, OperatorUserIDs: []int64{7002, 7003}}
	for id, want := range map[int64]bool{7001: true, 7002: true, 7003: true, 7004: false, 0: false, -7001: false} {
		if got := tracker.OperatorAllowed(id); got != want {
			t.Fatalf("OperatorAllowed(%d) = %v, want %v", id, got, want)
		}
	}
	if (TrackerConfig{AllowedCreatorID: 7001}).OperatorAllowed(7002) {
		t.Fatal("an unlisted user was allowed with no operator list")
	}
}

// The waits between attempts, and how long one stage goes on before the
// ticket is told once that it is still going. A configuration that says
// nothing gets the intended shape: a minute, growing to half an hour, and
// no limit at all on how many attempts a stage may make.
func TestTheRetrySettingsHaveWorkingDefaults(t *testing.T) {
	var unset ChainConfig
	if got := unset.RetryBackoffBase(); got != time.Minute {
		t.Fatalf("default first wait = %v, want a minute", got)
	}
	if got := unset.RetryBackoffMax(); got != 30*time.Minute {
		t.Fatalf("default longest wait = %v, want half an hour", got)
	}
	if got := unset.RetryNoticeAttemptsValue(); got != 3 {
		t.Fatalf("default attempts before the ticket is told = %d, want 3", got)
	}
	if unset.RetryMaxAttempts != 0 {
		t.Fatalf("default limit on attempts = %d, want none", unset.RetryMaxAttempts)
	}
	set := ChainConfig{RetryBackoffBaseSeconds: 5, RetryBackoffMaxSeconds: 90, RetryNoticeAttempts: 1}
	if set.RetryBackoffBase() != 5*time.Second || set.RetryBackoffMax() != 90*time.Second || set.RetryNoticeAttemptsValue() != 1 {
		t.Fatalf("configured values did not come back: %v %v %d",
			set.RetryBackoffBase(), set.RetryBackoffMax(), set.RetryNoticeAttemptsValue())
	}
}
