package attendant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/backlog"
	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/probe"
	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// designCommentHarness builds the least a postDesignComments run needs: a
// ledger with one claimed run bound to a ticket, a tracker whose uploads and
// comments this test can read back, and a run directory holding one sealed
// investigation and its measurements.
type designCommentHarness struct {
	config     runtime.Config
	services   *runtime.Services
	run        state.RunOverview
	uploads    *[]string
	comments   *[]string
	uploadFail *bool
}

func newDesignCommentHarness(t *testing.T, investigation investigate.Investigation, measurements int) designCommentHarness {
	t.Helper()
	root := t.TempDir()
	config := runtime.Config{
		Tracker: runtime.TrackerConfig{SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1},
		Identity: runtime.IdentityConfig{RepositoryID: 1, Repository: "o/r", WorkflowRef: "o/r/wf@main", EngineSHA: strings.Repeat("a", 40)},
		Chain:    runtime.ChainConfig{RunsRoot: filepath.Join(root, "runs")},
	}
	store, err := state.NewLocalStore(filepath.Join(root, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	envelope, err := hook.SealSnapshot(hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion, SpaceKey: "example", ActivityID: 9001, ActivityType: 1,
		ProjectID: 42, ProjectKey: "TICKET", IssueID: 30, IssueKey: "TICKET-781", IssueKeyID: 781, CreatorID: 7,
		RunID: "TICKET-781", CreatedAt: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), Target: config.Target(),
		Untrusted: hook.UntrustedTicketData{Summary: "s", Description: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), hook.QueueRequest{Envelope: envelope, QueuedAt: time.Date(2026, 9, 15, 0, 0, 1, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}

	uploads, comments, uploadFail := &[]string{}, &[]string{}, new(bool)
	next := int64(0)
	client, err := backlog.NewClient(backlog.Config{SpaceKey: "example", APIKey: "k", Origin: "https://example.backlog.com", Timeout: time.Second, MaxResponseBytes: 1 << 20},
		roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := []byte(nil)
			if r.Body != nil {
				read, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				body = read
			}
			switch {
			case r.URL.Path == "/api/v2/space/attachment":
				name := multipartFilename(string(body))
				if *uploadFail && name == hook.InvestigationReportFilename {
					return &http.Response{StatusCode: 500, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{}`))}, nil
				}
				*uploads = append(*uploads, name)
				next++
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%d}`, next)))}, nil
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
				values, _ := url.ParseQuery(string(body))
				*comments = append(*comments, values.Get("content"))
				encoded, _ := json.Marshal(map[string]any{"id": 9, "issueId": 30, "content": values.Get("content"),
					"createdUser": map[string]any{"id": 1}, "created": "2026-09-15T12:00:00Z"})
				return &http.Response{StatusCode: 201, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader("[]"))}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	slogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	target := config.Target()
	hookService, err := hook.NewService(hook.Config{
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1,
		RunMarker: "Automation-Run-ID", ExpectedRunID: "TICKET-781", Target: target, MaxEnvelopeBytes: 60 * 1024,
	}, client, store, slogger)
	if err != nil {
		t.Fatal(err)
	}
	route := hook.ReportRouteConfig{
		HMACKey: []byte(strings.Repeat("k", 32)), RepositoryID: 1,
		RepositorySHA256: hook.HashIdentity("o/r"), WorkflowRefSHA256: hook.HashIdentity("o/r/wf@main"),
		ExpectedRunID: "TICKET-781",
		Destinations: []hook.ReportDestination{{Repository: "example/consumer", Delivery: hook.DeliverPullRequest,
			StagingOrigin: "https://staging.example.test", ProductionOrigin: "https://www.example.test"}},
		ClockSkew: 2 * time.Minute, LeaseDuration: 2 * time.Minute,
		SpaceKey: "example", ProjectID: 42, ProjectKey: "TICKET", AllowedCreatorID: 7, AllowedActivityType: 1,
		Target: target, RunReferenceScheme: "local",
	}
	reportService, err := hook.NewTerminalReportService(route, store, client, slogger)
	if err != nil {
		t.Fatal(err)
	}
	tick, err := hook.NewQuestionTickService(route, store, client, reportService, hookService, readingStub{}, slogger)
	if err != nil {
		t.Fatal(err)
	}

	runDir := runDirectory(config, envelope.DeliveryID)
	if err := os.MkdirAll(designRoundDir(runDir, 1), 0o700); err != nil {
		t.Fatal(err)
	}
	investigation.MeasurementsCount = measurements
	encoded, err := json.Marshal(investigation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(designRoundDir(runDir, 1), "investigation.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	seedMeasurements(t, runDir, investigation)

	return designCommentHarness{
		config:   config,
		services: &runtime.Services{Store: store, Backlog: client, Tick: tick},
		run:      state.RunOverview{RunID: "TICKET-781", DeliveryID: envelope.DeliveryID},
		uploads:  uploads, comments: comments, uploadFail: uploadFail,
	}
}

// multipartFilename is the filename of a single-part multipart upload.
func multipartFilename(body string) string {
	_, rest, found := strings.Cut(body, `filename="`)
	if !found {
		return ""
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

// seedMeasurements seals one cited measurement per finding so the upload has
// something real to attach and the slot arithmetic has something to spend.
func seedMeasurements(t *testing.T, runDir string, investigation investigate.Investigation) {
	t.Helper()
	recorder, err := probe.OpenRecorder(filepath.Join(runDir, "measurements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < investigation.MeasurementsCount; index++ {
		if _, err := recorder.Append(probe.Measurement{Probe: "read_file", Output: "line\n"}); err != nil {
			t.Fatal(err)
		}
	}
}

// citedInvestigation is a report whose findings cite the measurements the
// harness seals, with claims of claimBytes each.
func citedInvestigation(claimBytes, findings int) investigate.Investigation {
	investigation := investigate.Investigation{Round: 1, Questions: []string{"Where does the count come from?"}, Next: "Change the query."}
	for index := 0; index < findings; index++ {
		id := fmt.Sprintf("m-%04d", index+1)
		investigation.Findings = append(investigation.Findings, investigate.Finding{
			Claim: "finding-" + id + " " + strings.Repeat("x", claimBytes), Confidence: investigate.ConfidenceMeasured,
			Evidence: []string{id},
		})
		investigation.Unknowns = append(investigation.Unknowns, "unknown-"+id+" "+strings.Repeat("y", 250))
	}
	return investigation
}

// A report too long for one comment is uploaded whole before the
// measurements are, so it cannot lose its place in the tracker's ten
// attachments, and the posted comment names the file for what it left out.
func TestPostDesignCommentsAttachesAnOversizeReport(t *testing.T) {
	investigation := citedInvestigation(600, 20)
	harness := newDesignCommentHarness(t, investigation, 20)
	postDesignComments(context.Background(), harness.config, harness.services, harness.run,
		chainView{designRound: 1}, runtime.ChainPlan{Shape: runtime.ShapeInvestigation}, &recordingLogger{})

	uploads := *harness.uploads
	if len(uploads) == 0 || uploads[0] != hook.InvestigationReportFilename {
		t.Fatalf("the report was not uploaded first: %v", uploads)
	}
	if len(uploads) != maxCommentAttachments {
		t.Fatalf("uploaded %d files; the tracker binds %d to one comment: %v", len(uploads), maxCommentAttachments, uploads)
	}
	if uploads[1] != "measurements-index.jsonl" {
		t.Fatalf("the measurements index lost its place: %v", uploads)
	}
	// The report took one of the ten, so eight raw outputs fit beside it and
	// the index rather than nine.
	if got := len(uploads) - 2; got != maxCommentAttachments-2 {
		t.Fatalf("attached %d raw outputs; the report's slot was not reserved", got)
	}
	comments := *harness.comments
	if len(comments) != 1 {
		t.Fatalf("posted %d comments", len(comments))
	}
	if !strings.Contains(comments[0], hook.InvestigationReportFilename) {
		t.Fatalf("the comment does not name the attachment:\n%s", comments[0])
	}
	if !strings.Contains(comments[0], investigation.Findings[0].Claim) {
		t.Fatalf("the comment carries no finding at all:\n%s", comments[0])
	}
	if len(comments[0]) > hook.MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(comments[0]), hook.MaxTrackerCommentBytes)
	}
	if !strings.Contains(comments[0], fmt.Sprintf("添付ファイル %d 件", maxCommentAttachments)) {
		t.Fatalf("the comment miscounts its attachments:\n%s", comments[0])
	}
}

// An upload the tracker refuses must not cost the requester the comment: it
// still posts, and it says the report waits in the run record rather than
// naming a file that is not there.
func TestPostDesignCommentsPostsWhenTheReportCannotBeAttached(t *testing.T) {
	investigation := citedInvestigation(600, 20)
	harness := newDesignCommentHarness(t, investigation, 20)
	*harness.uploadFail = true
	postDesignComments(context.Background(), harness.config, harness.services, harness.run,
		chainView{designRound: 1}, runtime.ChainPlan{Shape: runtime.ShapeInvestigation}, &recordingLogger{})

	for _, name := range *harness.uploads {
		if name == hook.InvestigationReportFilename {
			t.Fatal("a refused upload was counted as attached")
		}
	}
	comments := *harness.comments
	if len(comments) != 1 {
		t.Fatalf("posted %d comments; a failed attachment must not cost the report", len(comments))
	}
	if strings.Contains(comments[0], hook.InvestigationReportFilename) {
		t.Fatalf("the comment names a file that was never uploaded:\n%s", comments[0])
	}
	if !strings.Contains(comments[0], "実行記録から取り出します") {
		t.Fatalf("the comment does not say where the rest of the report is:\n%s", comments[0])
	}
	if len(comments[0]) > hook.MaxTrackerCommentBytes {
		t.Fatalf("comment is %d bytes; the tracker takes %d", len(comments[0]), hook.MaxTrackerCommentBytes)
	}
}

// A report that fits is shown whole and attaches no report file, so the
// measurements keep the whole budget.
func TestPostDesignCommentsLeavesAFittingReportWhole(t *testing.T) {
	investigation := citedInvestigation(120, 12)
	harness := newDesignCommentHarness(t, investigation, 12)
	postDesignComments(context.Background(), harness.config, harness.services, harness.run,
		chainView{designRound: 1}, runtime.ChainPlan{Shape: runtime.ShapeInvestigation}, &recordingLogger{})

	for _, name := range *harness.uploads {
		if name == hook.InvestigationReportFilename {
			t.Fatalf("a report that fits was attached anyway: %v", *harness.uploads)
		}
	}
	comments := *harness.comments
	if len(comments) != 1 {
		t.Fatalf("posted %d comments", len(comments))
	}
	for _, finding := range investigation.Findings {
		if !strings.Contains(comments[0], finding.Claim) {
			t.Fatalf("the comment lacks a finding whole:\n%s", comments[0])
		}
	}
	if strings.Contains(comments[0], "収まらない") {
		t.Fatalf("a report that fits was reported as shortened:\n%s", comments[0])
	}
}
