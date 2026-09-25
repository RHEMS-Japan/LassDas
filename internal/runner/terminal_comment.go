package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
	"automation.internal/ticket-ingress/internal/runtime"
)

// TerminalCommentFile is the closing comment a run decided to post, written
// into the run directory at the moment the ending was decided and before
// anything was sent.
//
// It exists because the comment used to have no existence of its own. The
// ending was recorded in the ledger as a digest, and the words were
// assembled again, from the board's cards and the run's artifacts, every
// time they were needed. That works until the pieces are gone: a pod that
// stopped between the ledger write and the tracker post leaves a run whose
// ending is decided and whose cards may have been swept, and the rebuild
// then cannot reproduce the digest the ledger holds. The requester was left
// with no comment at all, for ever, while an operator was told in a log.
//
// So the words are kept where the ending was decided, with the digest they
// belong to, and a re-send posts them as they were rather than making them
// again.
const TerminalCommentFile = "terminal-comment.json"

// maxTerminalCommentRecordBytes bounds the file on the way in. The body
// inside it is held to the tracker's own limit and the report beside it to
// the envelope's, so twice their sum is room to spare for the encoding and
// still refuses a file that is not this.
const maxTerminalCommentRecordBytes = 2 * (hook.MaxTrackerCommentBytes + hook.MaxTerminalReportRequestBytes)

// TerminalCommentRecord is one run's closing comment as it was decided.
//
// Body is the whole comment, footer and marker included, exactly as it
// would be posted. Report is what the ledger was asked to seal, and
// ReportSHA256 is the digest it sealed — which is also what the row holds.
// The digest is not believed: it is taken again over the report below, and
// a file whose parts disagree is not posted.
//
// The report's own prose — the run record, the cost line, what the
// delivery made possible — is left out of it. None of that is part of the
// sealed record, all of it is already in the body, and keeping a second
// copy would only be a second thing to drift.
type TerminalCommentRecord struct {
	Code         string                     `json:"code"`
	ReportSHA256 string                     `json:"report_sha256"`
	Marker       string                     `json:"marker"`
	Body         string                     `json:"body"`
	Report       hook.TerminalReportRequest `json:"report"`
	RecordedAt   time.Time                  `json:"recorded_at"`
}

// persistTerminalComment writes down the comment this report is about to
// post, before it is posted.
//
// Best-effort on purpose. A run directory that cannot be written is a
// reason to take the older path — rebuild, or failing that the recovery
// comment — not a reason to end the delivery with nothing on the ticket.
// The failure is logged because a directory that refuses this write is
// about to refuse the run's other records too.
func (t *Terminal) persistTerminalComment(report hook.TerminalReportRequest) {
	stamped := report
	// The shape check wants a timestamp, as every attempt carries one. It
	// is not part of the record, so which one this is does not matter.
	stamped.IssuedAt = time.Now().UTC()
	record, err := hook.MarshalTerminalReportRecord(stamped)
	if err != nil {
		t.logger.Error("closing comment not written down", "reason", "report shape invalid: "+err.Error())
		return
	}
	digest := hook.TerminalReportDigest(record)
	// Rendered by the same builder the report service posts with, so the
	// kept copy and the posted one are the same words — including whatever
	// the card that wrote each part had already masked out of it.
	body := hook.TerminalCommentContent(stamped, digest)
	// The words are kept; the prose the words were made from is not.
	bare := stamped
	bare.TrailText, bare.SpendText, bare.OutcomeText, bare.AssumptionsText = "", "", "", ""
	kept := TerminalCommentRecord{
		Code: string(stamped.Code), ReportSHA256: digest,
		Marker: hook.ExtractCommentMarker(body), Body: body,
		Report: bare, RecordedAt: stamped.IssuedAt,
	}
	if err := kept.validate(); err != nil {
		t.logger.Error("closing comment not written down", "reason", err.Error())
		return
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		t.logger.Error("closing comment not written down", "reason", "record unencodable")
		return
	}
	path := filepath.Join(t.workspace, TerminalCommentFile)
	// Removed first for the reason every record here is: a link left at
	// the path must not carry the write somewhere else.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.logger.Error("closing comment not written down", "reason", "path not clearable")
		return
	}
	if err := writeRecordAtomically(path, encoded); err != nil {
		t.logger.Error("closing comment not written down", "reason", err.Error())
		return
	}
	t.logger.Info("closing comment written down", "code", string(stamped.Code), "digest", digest)
}

// ReadTerminalComment reads back the closing comment a run decided to post,
// or reports that there is none to read. A file that is absent, torn or
// inconsistent is the same answer as no file: the caller falls back to
// rebuilding the report, and to the recovery comment after that.
func ReadTerminalComment(runDir string) (TerminalCommentRecord, bool) {
	path := filepath.Join(runDir, TerminalCommentFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxTerminalCommentRecordBytes {
		return TerminalCommentRecord{}, false
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return TerminalCommentRecord{}, false
	}
	var kept TerminalCommentRecord
	if err := json.Unmarshal(encoded, &kept); err != nil {
		return TerminalCommentRecord{}, false
	}
	if kept.validate() != nil {
		return TerminalCommentRecord{}, false
	}
	return kept, true
}

// validate holds the record to what makes it safe to post without reading
// anything else: a body the tracker will accept, a marker that is really
// the marker on that body, and a digest the sealed record actually hashes
// to. The last one is the binding — a body whose digest was written down
// from somewhere other than its own record could be posted against a
// different ending.
func (r TerminalCommentRecord) validate() error {
	if r.Body == "" || len(r.Body) > hook.MaxTrackerCommentBytes {
		return errors.New("the closing comment is empty or over the tracker's limit")
	}
	if r.Marker == "" || hook.ExtractCommentMarker(r.Body) != r.Marker {
		return errors.New("the closing comment does not end with the marker it names")
	}
	if !hook.TerminalCode(r.Code).Valid() || string(r.Report.Code) != r.Code {
		return errors.New("the closing comment and its report name different endings")
	}
	stamped := r.Report
	stamped.IssuedAt = time.Now().UTC()
	record, err := hook.MarshalTerminalReportRecord(stamped)
	if err != nil {
		return fmt.Errorf("the report beside the closing comment is not a report: %w", err)
	}
	// The binding. A body kept beside a report that seals to some other
	// digest is a body that could be posted against a different ending, so
	// it is refused and the caller reports from the ledger instead.
	if hook.TerminalReportDigest(record) != r.ReportSHA256 {
		return errors.New("the closing comment's digest is not its report's")
	}
	return nil
}

// Request is the report the ledger sealed, ready to be sent again. The
// timestamp is this attempt's; everything else is what was sealed, so the
// digest the store computes is the one the row holds.
func (r TerminalCommentRecord) Request() (hook.TerminalReportRequest, error) {
	if err := r.validate(); err != nil {
		return hook.TerminalReportRequest{}, err
	}
	request := r.Report
	request.IssuedAt = time.Now().UTC()
	return request, nil
}

// ResubmitPersistedComment sends the closing comment a run wrote down when
// it decided its ending, as it was written.
//
// Nothing is rebuilt and nothing else is read: the report the ledger is
// asked to complete is the sealed record kept beside the comment, and the
// words posted are the kept ones. That is the whole point — this runs for
// deliveries whose board cards and artifacts are gone, and a path that
// needed the run's envelope or its dispatch identity would fail for the
// same reason the rebuild does.
func ResubmitPersistedComment(ctx context.Context, services *runtime.Services, kept TerminalCommentRecord, logger TerminalLogger) error {
	if services == nil || services.Report == nil {
		return errors.New("this deployment has no report service")
	}
	request, err := kept.Request()
	if err != nil {
		return err
	}
	return submitWithRetry(ctx, logger, "kept terminal report", func(issuedAt time.Time) (hook.Result, error) {
		request.IssuedAt = issuedAt
		return services.Report.ProcessPersistedTerminalReport(ctx, request, kept.Body), nil
	})
}
