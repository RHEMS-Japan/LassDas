package attendant

import (
	"encoding/json"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/runner"
	"automation.internal/ticket-ingress/internal/worker"
)

// Kept only to explain runs an older engine actually restarted. New runs
// restore the accepted decision without starting the reception again.
const receptionAgainSchemaVersion = 1

type receptionAgainRecord struct {
	SchemaVersion int       `json:"schema_version"`
	Reason        string    `json:"reason"`
	At            time.Time `json:"at"`
}

func receptionAgainAssumption(runDir string) string {
	raw, err := worker.ReadBoundedRegularFile(filepath.Join(runDir, runner.ReceptionAgainFile), 4096)
	if err != nil {
		return ""
	}
	var record receptionAgainRecord
	if json.Unmarshal(raw, &record) != nil || record.SchemaVersion != receptionAgainSchemaVersion {
		return ""
	}
	return "受付の判断を記録したファイルが読めなくなっていたため、受付をもう一度実行し、そこで出た判断を前提にしています。"
}
