package state

import (
	"context"
	"strconv"
	"testing"
)

// A row's integers must survive the JSON round-trip exactly. 2^53+1 is the
// first integer float64 cannot represent, and 7663335643410923834 is the
// live chain claim identity that came back 314 short and had every terminal
// report for its run refused as a conflict.
func TestLedgerRowInt64RoundTripIsExact(t *testing.T) {
	store := newLocalForTest(t)
	for _, value := range []int64{1, 1 << 53, (1 << 53) + 1, 7663335643410923834} {
		txn, err := store.begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		key := "precision#" + strconv.FormatInt(value, 10)
		if ok, err := txn.putNew(key, item{"value": value}); err != nil || !ok {
			t.Fatalf("putNew(%d) = %v, %v", value, ok, err)
		}
		if err := txn.commit(); err != nil {
			t.Fatal(err)
		}
		// Two read/rewrite cycles: the second pins the re-Marshal of a
		// json.Number-carrying row, not just the first decode.
		for cycle := 0; cycle < 2; cycle++ {
			txn, err := store.begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			row, err := txn.getItem(key)
			if err != nil || row == nil {
				t.Fatalf("getItem(%d) = %v, %v", value, row, err)
			}
			if !row.int64Equals("value", value) {
				got, _ := row.int64At("value")
				t.Fatalf("cycle %d of %d read back as %d", cycle, value, got)
			}
			if err := txn.setItem(key, row); err != nil {
				t.Fatal(err)
			}
			if err := txn.commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
}
