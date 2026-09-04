package runtime

import "testing"

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

func TestFailureStreakLimitDefaultsToThreeAndCanBeSwitchedOff(t *testing.T) {
	if got := (ChainConfig{}).FailureStreakLimitValue(); got != 3 {
		t.Fatalf("default limit = %d, want 3", got)
	}
	off := 0
	if got := (ChainConfig{FailureStreakLimit: &off}).FailureStreakLimitValue(); got != 0 {
		t.Fatalf("explicit 0 = %d, want 0", got)
	}
	five := 5
	if got := (ChainConfig{FailureStreakLimit: &five}).FailureStreakLimitValue(); got != 5 {
		t.Fatalf("explicit 5 = %d, want 5", got)
	}
}
