package worker

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/hook"
)

const (
	maxTicketSummaryBytes     = 240
	maxTicketDescriptionBytes = 32 * 1024
	maxTicketRequestBytes     = 16 * 1024
	maxTicketHeaderBytes      = 1024
	minAcceptanceTextBytes    = 8
)

type TicketRequest struct {
	SchemaVersion    int      `json:"schema_version"`
	DeliveryID       string   `json:"delivery_id"`
	InputSHA256      string   `json:"input_sha256"`
	ConfigSHA256     string   `json:"config_sha256"`
	ToolSHA          string   `json:"tool_sha"`
	IssueKey         string   `json:"issue_key"`
	RunID            string   `json:"run_id"`
	Repository       string   `json:"repository"`
	Mode             string   `json:"mode"`
	Summary          string   `json:"summary"`
	TargetFiles      []string `json:"target_files"`
	VerificationPath string   `json:"verification_path"`
	ExpectedText     string   `json:"expected_text"`
	AbsentText       string   `json:"absent_text"`
	Request          string   `json:"request"`
}

// Consumer resolves the delivery destination this ticket is bound to.
func (r TicketRequest) Consumer(config Config) (ConsumerConfig, error) {
	return config.ConsumerFor(r.Repository)
}

// TicketDraft is a ticket whose target files are not yet determined. The
// requester stated what should change, what it should become and where to see
// it — everything they can know without reading the repository. Which file
// implements it is the automation's job to work out, so a draft carries every
// TicketRequest field except TargetFiles.
type TicketDraft struct {
	SchemaVersion    int    `json:"schema_version"`
	DeliveryID       string `json:"delivery_id"`
	InputSHA256      string `json:"input_sha256"`
	ConfigSHA256     string `json:"config_sha256"`
	ToolSHA          string `json:"tool_sha"`
	IssueKey         string `json:"issue_key"`
	RunID            string `json:"run_id"`
	Repository       string `json:"repository"`
	Mode             string `json:"mode"`
	Summary          string `json:"summary"`
	VerificationPath string `json:"verification_path"`
	ExpectedText     string `json:"expected_text"`
	AbsentText       string `json:"absent_text"`
	Request          string `json:"request"`
}

const unboundDevelopmentToolSHA = "0000000000000000000000000000000000000000"

// ParseTicket is retained for library/test compatibility. Production entry
// points must call ParseTicketWithToolSHA so the workflow revision is explicit.
func ParseTicket(envelope hook.DispatchEnvelope, config Config) (TicketRequest, error) {
	return ParseTicketWithToolSHA(envelope, config, unboundDevelopmentToolSHA)
}

// ParseTicketWithToolSHA converts an immutable envelope into the first artifact
// in the worker chain and binds it to both configuration and tool revisions.
func ParseTicketWithToolSHA(envelope hook.DispatchEnvelope, config Config, toolSHA string) (TicketRequest, error) {
	draft, targetFiles, err := ParseTicketDraft(envelope, config, toolSHA)
	if err != nil {
		return TicketRequest{}, err
	}
	if len(targetFiles) == 0 {
		return TicketRequest{}, errors.New("ticket target file count is invalid")
	}
	return draft.WithTargetFiles(targetFiles, config)
}

// ParseTicketDraft converts an immutable envelope into a draft plus whatever
// target files the requester chose to name. Naming none is allowed: the files
// are then derived before the contract is completed.
func ParseTicketDraft(envelope hook.DispatchEnvelope, config Config, toolSHA string) (TicketDraft, []string, error) {
	if err := config.Validate(); err != nil {
		return TicketDraft{}, nil, errors.New("worker configuration is invalid")
	}
	// The fixed-header form has no repository line; it can only ever bind a
	// single-destination configuration.
	consumer, sole := config.SoleConsumer()
	if !sole {
		return TicketDraft{}, nil, errors.New("fixed-header tickets require exactly one configured consumer")
	}
	configSHA, err := config.SHA256()
	if err != nil || !ValidToolSHA(toolSHA) {
		return TicketDraft{}, nil, errors.New("worker artifact identity is invalid")
	}
	if err := hook.ValidateEnvelope(envelope); err != nil {
		return TicketDraft{}, nil, errors.New("ticket envelope is invalid")
	}
	snapshot := envelope.Snapshot
	if err := validatePlainText(snapshot.Untrusted.Summary, maxTicketSummaryBytes, false); err != nil {
		return TicketDraft{}, nil, errors.New("ticket summary is invalid")
	}
	description := snapshot.Untrusted.Description
	if len(description) == 0 || len(description) > maxTicketDescriptionBytes || !utf8.ValidString(description) || strings.ContainsRune(description, '\x00') {
		return TicketDraft{}, nil, errors.New("ticket description is invalid")
	}
	description = strings.ReplaceAll(description, "\r\n", "\n")
	if strings.ContainsRune(description, '\r') || hasDisallowedControls(description, true) {
		return TicketDraft{}, nil, errors.New("ticket description is invalid")
	}
	lines := strings.Split(description, "\n")
	if len(lines) < 7 {
		return TicketDraft{}, nil, errors.New("ticket contract is incomplete")
	}
	for _, line := range lines {
		if len(line) > maxTicketHeaderBytes && line != "---" {
			return TicketDraft{}, nil, errors.New("ticket line is too long")
		}
	}

	index := 0
	runID, err := exactHeader(lines[index], "Automation-Run-ID")
	if err != nil || runID != snapshot.RunID {
		return TicketDraft{}, nil, errors.New("ticket run id is invalid")
	}
	index++
	mode, err := exactHeader(lines[index], "Automation-Mode")
	if err != nil || mode != consumer.Mode.ID {
		return TicketDraft{}, nil, errors.New("ticket mode is invalid")
	}
	index++

	targetFiles := make([]string, 0, consumer.Mode.MaxFiles)
	seenFiles := make(map[string]struct{}, consumer.Mode.MaxFiles)
	for index < len(lines) && strings.HasPrefix(lines[index], "Target-File: ") {
		filename, headerErr := exactHeader(lines[index], "Target-File")
		if headerErr != nil || !validRelativePath(filename) || !allowedPath(filename, consumer.Mode.AllowedFilePrefixes) {
			return TicketDraft{}, nil, errors.New("ticket target file is invalid")
		}
		if _, duplicate := seenFiles[filename]; duplicate {
			return TicketDraft{}, nil, errors.New("ticket target files contain duplicates")
		}
		seenFiles[filename] = struct{}{}
		targetFiles = append(targetFiles, filename)
		index++
	}
	if len(targetFiles) > consumer.Mode.MaxFiles {
		return TicketDraft{}, nil, errors.New("ticket target file count is invalid")
	}
	if index >= len(lines) {
		return TicketDraft{}, nil, errors.New("ticket contract is incomplete")
	}

	verificationPath, err := exactHeader(lines[index], "Verification-Path")
	if err != nil || !validVerificationPath(verificationPath) {
		return TicketDraft{}, nil, errors.New("ticket verification path is invalid")
	}
	index++
	if index >= len(lines) {
		return TicketDraft{}, nil, errors.New("ticket contract is incomplete")
	}
	expectedText, err := exactHeader(lines[index], "Expected-Text")
	if err != nil || validateAcceptanceText(expectedText) != nil {
		return TicketDraft{}, nil, errors.New("ticket expected text is invalid")
	}
	index++

	if index >= len(lines) {
		return TicketDraft{}, nil, errors.New("ticket contract is incomplete")
	}
	absentText, err := exactHeader(lines[index], "Absent-Text")
	if err != nil || validateAcceptanceText(absentText) != nil || absentText == expectedText {
		return TicketDraft{}, nil, errors.New("ticket absent text is invalid")
	}
	index++
	if index >= len(lines) || lines[index] != "---" {
		return TicketDraft{}, nil, errors.New("ticket separator is invalid")
	}
	index++
	// Blank lines around the body carry no meaning and are normalised rather
	// than rejected: a ticket written in Backlog almost always ends with a
	// newline, and refusing it would tell the requester only that the input
	// was "not in the permitted format".
	requestBody := strings.TrimSpace(strings.Join(lines[index:], "\n"))
	if len(requestBody) == 0 || len(requestBody) > maxTicketRequestBytes || hasDisallowedControls(requestBody, true) {
		return TicketDraft{}, nil, errors.New("ticket request is invalid")
	}

	draft := TicketDraft{
		SchemaVersion: 1, DeliveryID: envelope.DeliveryID, InputSHA256: snapshot.InputSHA256,
		ConfigSHA256: configSHA, ToolSHA: toolSHA,
		IssueKey: snapshot.IssueKey, RunID: snapshot.RunID, Repository: consumer.Repository,
		Mode: mode, Summary: snapshot.Untrusted.Summary,
		VerificationPath: verificationPath, ExpectedText: expectedText,
		AbsentText: absentText, Request: requestBody,
	}
	sort.Strings(targetFiles)
	return draft, targetFiles, nil
}

// WithTargetFiles completes a draft once the target files are known, applying
// exactly the same validation a fully written ticket receives.
func (d TicketDraft) WithTargetFiles(targetFiles []string, config Config) (TicketRequest, error) {
	files := append([]string(nil), targetFiles...)
	sort.Strings(files)
	request := TicketRequest{
		SchemaVersion: d.SchemaVersion, DeliveryID: d.DeliveryID, InputSHA256: d.InputSHA256,
		ConfigSHA256: d.ConfigSHA256, ToolSHA: d.ToolSHA,
		IssueKey: d.IssueKey, RunID: d.RunID, Repository: d.Repository, Mode: d.Mode, Summary: d.Summary,
		TargetFiles: files, VerificationPath: d.VerificationPath, ExpectedText: d.ExpectedText,
		AbsentText: d.AbsentText, Request: d.Request,
	}
	if err := request.Validate(config); err != nil {
		return TicketRequest{}, errors.New("ticket request is invalid")
	}
	return request, nil
}

func (r TicketRequest) Validate(config Config) error {
	configSHA, err := config.SHA256()
	if err != nil {
		return errors.New("worker configuration is invalid")
	}
	consumer, err := config.ConsumerFor(r.Repository)
	if err != nil {
		return errors.New("ticket repository is not a configured consumer")
	}
	if r.SchemaVersion != 1 || !deliveryPattern.MatchString(r.DeliveryID) || !sha256Pattern.MatchString(r.InputSHA256) ||
		r.ConfigSHA256 != configSHA || !ValidToolSHA(r.ToolSHA) ||
		!issueKeyPattern.MatchString(r.IssueKey) || !runIDPattern.MatchString(r.RunID) || r.Mode != consumer.Mode.ID {
		return errors.New("ticket identity is invalid")
	}
	if err := validatePlainText(r.Summary, maxTicketSummaryBytes, false); err != nil {
		return errors.New("ticket summary is invalid")
	}
	if len(r.TargetFiles) == 0 || len(r.TargetFiles) > consumer.Mode.MaxFiles || !sort.StringsAreSorted(r.TargetFiles) {
		return errors.New("ticket target files are invalid")
	}
	seen := make(map[string]struct{}, len(r.TargetFiles))
	for _, filename := range r.TargetFiles {
		if !validRelativePath(filename) || !allowedPath(filename, consumer.Mode.AllowedFilePrefixes) {
			return errors.New("ticket target file is invalid")
		}
		if _, exists := seen[filename]; exists {
			return errors.New("ticket target files contain duplicates")
		}
		seen[filename] = struct{}{}
	}
	if err := validateWordingPromise(r.VerificationPath, r.ExpectedText, r.AbsentText); err != nil {
		return err
	}
	if len(r.Request) == 0 || len(r.Request) > maxTicketRequestBytes || strings.TrimSpace(r.Request) != r.Request ||
		!utf8.ValidString(r.Request) || hasDisallowedControls(r.Request, true) {
		return errors.New("ticket request is invalid")
	}
	return nil
}

func validateAcceptanceText(value string) error {
	if len(value) < minAcceptanceTextBytes || validatePlainText(value, 512, false) != nil {
		return errors.New("acceptance text is invalid")
	}
	return nil
}

// HasWordingPromise reports whether this ticket promises a visible wording
// change. When it does not, the wording-specific checks (the absent text
// exists today, the expected text exists afterwards, the browser observation)
// do not apply; the deterministic verification commands still do.
func (r TicketRequest) HasWordingPromise() bool {
	return r.VerificationPath != "" || r.ExpectedText != "" || r.AbsentText != ""
}

// validateWordingPromise accepts either a complete visible-wording promise or
// none at all. A partial promise would let a later step verify half of what
// was asked.
func validateWordingPromise(verificationPath, expectedText, absentText string) error {
	if verificationPath == "" && expectedText == "" && absentText == "" {
		return nil
	}
	if !validVerificationPath(verificationPath) || validateAcceptanceText(expectedText) != nil {
		return errors.New("ticket verification contract is invalid")
	}
	if validateAcceptanceText(absentText) != nil || absentText == expectedText {
		return errors.New("ticket absent text is invalid")
	}
	return nil
}

func exactHeader(line, name string) (string, error) {
	prefix := name + ": "
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("missing %s header", name)
	}
	value := strings.TrimPrefix(line, prefix)
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, ": ") && strings.HasPrefix(value, name) || hasDisallowedControls(value, false) {
		return "", fmt.Errorf("invalid %s header", name)
	}
	return value, nil
}

func validVerificationPath(value string) bool {
	if len(value) == 0 || len(value) > 256 || !verificationPathPattern.MatchString(value) || strings.HasPrefix(value, "//") ||
		strings.ContainsAny(value, "\\?#%\r\n\x00") || hasDisallowedControls(value, false) {
		return false
	}
	cleaned := path.Clean(value)
	if value == "/" {
		return true
	}
	return cleaned == value && !strings.Contains(value, "/../") && !strings.HasSuffix(value, "/..")
}

func validatePlainText(value string, maxBytes int, allowNewlines bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value || hasDisallowedControls(value, allowNewlines) {
		return errors.New("plain text is invalid")
	}
	if !allowNewlines && strings.ContainsAny(value, "\r\n") {
		return errors.New("plain text contains a newline")
	}
	return nil
}

func hasDisallowedControls(value string, allowNewlines bool) bool {
	for _, r := range value {
		if r == '\t' || allowNewlines && r == '\n' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
