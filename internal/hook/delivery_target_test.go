package hook

import (
	"strings"
	"testing"
)

func TestSameDeliveryNamesTheRepositoryNotTheRevision(t *testing.T) {
	base := DeliveryTarget{RepositoryID: 42, WorkflowRefSHA256: strings.Repeat("a", 64)}
	newer := DeliveryTarget{RepositoryID: 42, WorkflowRefSHA256: strings.Repeat("b", 64)}
	other := DeliveryTarget{RepositoryID: 43, WorkflowRefSHA256: base.WorkflowRefSHA256}
	if !base.SameDelivery(newer) {
		t.Fatal("a newer revision of the same delivery was treated as another delivery")
	}
	if base.SameDelivery(other) {
		t.Fatal("another repository was treated as the same delivery")
	}
}
