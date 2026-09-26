package attendant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditUnsettledPlanDoesNotPublishDiscardedAssumptions(t *testing.T) {
	for _, marker := range []string{"fallback", "inconclusive_reading"} {
		t.Run(marker, func(t *testing.T) {
			dir := t.TempDir()
			readiness := filepath.Join(dir, "history", "readiness")
			if err := os.MkdirAll(readiness, 0755); err != nil {
				t.Fatal(err)
			}
			for name, body := range map[string]string{
				"decision.json":     `{"outcome":"ready","` + marker + `":true}`,
				"assessment-1.json": `{"assumptions":[{"statement":"discarded assumption"},{"kind":"defensible_default","statement":"discarded decision"}]}`,
			} {
				if err := os.WriteFile(filepath.Join(readiness, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			facts := loadPlanFacts(dir)
			if strings.Contains(strings.Join(facts.Assumptions, "\n"), "discarded assumption") {
				t.Fatalf("discarded reading published: %+v", facts)
			}
			if len(facts.Decided) != 0 {
				t.Fatalf("an unverified interpretation became a decision: %+v", facts)
			}
		})
	}
}
