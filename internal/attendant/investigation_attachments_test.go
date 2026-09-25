package attendant

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// uploadingClient is a tracker client that accepts every upload and records
// the filenames it was given.
func uploadingClient(t *testing.T, uploaded *[]string) *backlog.Client {
	t.Helper()
	next := int64(0)
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/api/v2/space/attachment" {
				return &http.Response{StatusCode: 404, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range strings.Split(string(body), "filename=\"") {
				if index := strings.Index(part, "\""); index > 0 && strings.Contains(part[:index], ".") {
					*uploaded = append(*uploaded, part[:index])
				}
			}
			next++
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%d}`, next)))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// measurementsFile seals n cited measurements in a run directory and returns
// the investigation that cites every one of them.
func measurementsFile(t *testing.T, runDir string, n int) investigate.Investigation {
	t.Helper()
	recorder, err := probe.OpenRecorder(runDir + "/measurements.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	investigation := investigate.Investigation{Round: 1, MeasurementsCount: n}
	for index := 0; index < n; index++ {
		measurement, err := recorder.Append(probe.Measurement{Probe: "read_file", Output: "line\n"})
		if err != nil {
			t.Fatal(err)
		}
		investigation.Findings = append(investigation.Findings, investigate.Finding{
			Claim: "finding " + measurement.ID, Confidence: investigate.ConfidenceMeasured,
			Evidence: []string{measurement.ID},
		})
	}
	return investigation
}

// The tracker takes ten attachments on one comment. When the report itself
// has to travel as an attachment it takes one of those slots, so the
// measurements have to be told they have one fewer -- otherwise the comment
// is refused outright and the requester gets nothing.
func TestMeasurementUploadsRespectTheSlotsLeftForTheReport(t *testing.T) {
	runDir := t.TempDir()
	investigation := measurementsFile(t, runDir, 15)
	var uploaded []string
	services := &runtime.Services{Backlog: uploadingClient(t, &uploaded)}

	ids, omitted := uploadMeasurements(context.Background(), services, runDir, investigation, maxCommentAttachments, &recordingLogger{})
	if len(ids) != maxCommentAttachments {
		t.Fatalf("a full budget attached %d files; the tracker takes %d", len(ids), maxCommentAttachments)
	}
	if omitted != 15-(maxCommentAttachments-1) {
		t.Fatalf("omitted = %d; the index takes one of the slots", omitted)
	}

	uploaded = nil
	reserved, omittedReserved := uploadMeasurements(context.Background(), services, runDir, investigation, maxCommentAttachments-1, &recordingLogger{})
	if len(reserved) != maxCommentAttachments-1 {
		t.Fatalf("a reserved budget attached %d files; one slot was held for the report", len(reserved))
	}
	if omittedReserved != omitted+1 {
		t.Fatalf("omitted = %d; the reserved slot must be counted as left out, not dropped in silence", omittedReserved)
	}
	if len(uploaded) == 0 || uploaded[0] != "measurements-index.jsonl" {
		t.Fatalf("uploaded = %v", uploaded)
	}

	if ids, omitted := uploadMeasurements(context.Background(), services, runDir, investigation, 0, &recordingLogger{}); ids != nil || omitted != 0 {
		t.Fatalf("a budget of zero attached %d files", len(ids))
	}
}
