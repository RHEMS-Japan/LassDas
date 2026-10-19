package diskusage

import "testing"

func TestPercentAndThreshold(t *testing.T) {
	for _, tc := range []struct {
		blocks, available uint64
		want              float64
		bad               bool
	}{
		{100, 100, 0, false}, {100, 15, 85, false}, {100, 0, 100, false},
		{0, 0, 0, true}, {100, 101, 0, true},
	} {
		got, err := percentOf(tc.blocks, tc.available)
		if got != tc.want || (err != nil) != tc.bad {
			t.Fatalf("%+v: %v %v", tc, got, err)
		}
	}
	if Threshold(nil) != 85 {
		t.Fatal("default")
	}
	for _, n := range []int{-1, 0, 85, 100, 101} {
		if (Validate(&n) != nil) != (n < 0 || n > 100) {
			t.Fatalf("validation %d", n)
		}
		if n >= 0 && n <= 100 && Threshold(&n) != n {
			t.Fatalf("threshold %d", n)
		}
	}
	if Over(100, 0) || Over(84.9, 85) || !Over(85, 85) || !Over(100, 100) {
		t.Fatal("boundary/off")
	}
}
