// Package investigate holds the investigating designer's sealed records:
// the investigation report and the design, one pair per round, bound to
// the measurements the kernel recorded (docs/INVESTIGATING_DESIGNER.md §4).
// The model proposes their content; the kernel seals them only after the
// checks here pass, and everything downstream references the fingerprints.
package investigate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"automation.internal/ticket-ingress/internal/probe"
)

// SchemaVersion is the records' schema version.
const SchemaVersion = 1

const (
	maxQuestions       = 8
	maxFindings        = 20
	maxUnknowns        = 20
	maxShortText       = 300
	maxLongText        = 600
	maxAlternatives    = 3
	maxChangeNotes     = 12
	maxBlastRadius     = 12
	maxNotDoing        = 12
	maxCauseEvidence   = 8
	maxFindingEvidence = 8
)

var (
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	measurementIDPattern = regexp.MustCompile(`^m-[0-9]{4}$`)
	relativePathPattern  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]{0,255}$`)
)

// Identity binds a record to the run that produced it: the same fields the
// other sealed artifacts carry, plus the baseline the working copy stood at.
type Identity struct {
	DeliveryID   string `json:"delivery_id"`
	InputSHA256  string `json:"input_sha256"`
	ConfigSHA256 string `json:"config_sha256"`
	ToolSHA      string `json:"tool_sha"`
	BaseSHA      string `json:"base_sha"`
}

func (id Identity) validate() error {
	if id.DeliveryID == "" || !sha256Pattern.MatchString(id.InputSHA256) || !sha256Pattern.MatchString(id.ConfigSHA256) ||
		id.ToolSHA == "" || !commitPattern.MatchString(id.BaseSHA) {
		return errors.New("record identity is invalid")
	}
	return nil
}

// Confidence values a finding may carry.
const (
	ConfidenceMeasured = "measured"
	ConfidenceInferred = "inferred"
)

// Finding is one claim with the measurements that support it.
type Finding struct {
	Claim      string   `json:"claim"`
	Evidence   []string `json:"evidence,omitempty"`
	Confidence string   `json:"confidence"`
}

// ModelInvestigationOutput is what the model answers with when it stops
// probing. Decoded strictly; validated against the measurements.
type ModelInvestigationOutput struct {
	Questions []string  `json:"questions"`
	Findings  []Finding `json:"findings"`
	Unknowns  []string  `json:"unknowns"`
	Next      string    `json:"next"`
}

// Investigation is the sealed report of one round.
type Investigation struct {
	SchemaVersion int `json:"schema_version"`
	Identity
	Round int `json:"round"`
	// MeasurementsCount and MeasurementsChainSHA256 fix the prefix of
	// measurements.jsonl this report stands on; later rounds append after
	// it without disturbing the check.
	MeasurementsCount       int    `json:"measurements_count"`
	MeasurementsChainSHA256 string `json:"measurements_chain_sha256"`
	// ProbesUsed and ElapsedSeconds are the budget spent so far, for the
	// next round to continue from.
	ProbesUsed     int `json:"probes_used"`
	ElapsedSeconds int `json:"elapsed_seconds"`

	Questions []string  `json:"questions"`
	Findings  []Finding `json:"findings"`
	Unknowns  []string  `json:"unknowns"`
	Next      string    `json:"next"`

	InvestigationSHA256 string `json:"investigation_sha256"`
}

// Budget is what the round spent, as the kernel counted it.
type Budget struct {
	ProbesUsed     int
	ElapsedSeconds int
}

// NewInvestigation validates the model's output against the first count
// lines of the measurements file, seals the record and returns it.
func NewInvestigation(identity Identity, round int, output ModelInvestigationOutput, measurementsPath string, count int, budget Budget) (Investigation, error) {
	if round < 1 {
		return Investigation{}, errors.New("investigation round must be positive")
	}
	if budget.ProbesUsed < 0 || budget.ElapsedSeconds < 0 {
		return Investigation{}, errors.New("investigation budget is invalid")
	}
	chain, err := probe.VerifyPrefix(measurementsPath, count)
	if err != nil {
		return Investigation{}, fmt.Errorf("measurements: %w", err)
	}
	record := Investigation{
		SchemaVersion: SchemaVersion, Identity: identity, Round: round,
		MeasurementsCount: count, MeasurementsChainSHA256: chain,
		ProbesUsed: budget.ProbesUsed, ElapsedSeconds: budget.ElapsedSeconds,
		Questions: append([]string(nil), output.Questions...),
		Findings:  append([]Finding(nil), output.Findings...),
		Unknowns:  append([]string(nil), output.Unknowns...),
		Next:      output.Next,
	}
	digest, err := investigationDigest(record)
	if err != nil {
		return Investigation{}, errors.New("investigation could not be sealed")
	}
	record.InvestigationSHA256 = digest
	if err := record.Validate(identity, measurementsPath); err != nil {
		return Investigation{}, err
	}
	return record, nil
}

// Validate re-derives everything a later stage relies on: identity, the
// measurements prefix, every measured finding's evidence, the fingerprint.
func (r Investigation) Validate(identity Identity, measurementsPath string) error {
	if r.SchemaVersion != SchemaVersion || r.Identity != identity || r.Round < 1 {
		return errors.New("investigation identity is invalid")
	}
	if err := identity.validate(); err != nil {
		return err
	}
	if r.ProbesUsed < 0 || r.ElapsedSeconds < 0 || r.MeasurementsCount < 0 || r.ProbesUsed < r.MeasurementsCount {
		return errors.New("investigation budget is invalid")
	}
	chain, err := probe.VerifyPrefix(measurementsPath, r.MeasurementsCount)
	if err != nil {
		return fmt.Errorf("measurements: %w", err)
	}
	if chain != r.MeasurementsChainSHA256 {
		return errors.New("investigation measurements chain does not match")
	}
	measurements, err := probe.ReadPrefix(measurementsPath, r.MeasurementsCount)
	if err != nil {
		return fmt.Errorf("measurements: %w", err)
	}
	usable := map[string]bool{}
	for _, measurement := range measurements {
		usable[measurement.ID] = !measurement.Refused
	}
	if err := validateInvestigationText(r.Questions, r.Findings, r.Unknowns, r.Next, usable); err != nil {
		return err
	}
	digest, err := investigationDigest(r)
	if err != nil || digest != r.InvestigationSHA256 {
		return errors.New("investigation digest is invalid")
	}
	return nil
}

// MeasuredEvidence lists every measurement id a measured finding cites.
func (r Investigation) MeasuredEvidence() map[string]bool {
	out := map[string]bool{}
	for _, finding := range r.Findings {
		if finding.Confidence == ConfidenceMeasured {
			for _, id := range finding.Evidence {
				out[id] = true
			}
		}
	}
	return out
}

func validateInvestigationText(questions []string, findings []Finding, unknowns []string, next string, usable map[string]bool) error {
	if len(questions) == 0 || len(questions) > maxQuestions || len(findings) > maxFindings || len(unknowns) > maxUnknowns {
		return errors.New("investigation has the wrong number of questions, findings or unknowns")
	}
	for _, question := range questions {
		if !validText(question, maxShortText) {
			return errors.New("investigation question is invalid")
		}
	}
	for _, unknown := range unknowns {
		if !validText(unknown, maxShortText) {
			return errors.New("investigation unknown is invalid")
		}
	}
	if !validText(next, maxLongText) {
		return errors.New("investigation next step is invalid")
	}
	for _, finding := range findings {
		if !validText(finding.Claim, maxLongText) || len(finding.Evidence) > maxFindingEvidence {
			return errors.New("investigation finding is invalid")
		}
		for _, id := range finding.Evidence {
			if !measurementIDPattern.MatchString(id) {
				return errors.New("investigation finding cites a malformed measurement id")
			}
		}
		switch finding.Confidence {
		case ConfidenceMeasured:
			if len(finding.Evidence) == 0 {
				return errors.New("measured finding cites no measurement")
			}
			for _, id := range finding.Evidence {
				ok, present := usable[id]
				if !present {
					return fmt.Errorf("measured finding cites %s, which is not among the sealed measurements", id)
				}
				if !ok {
					return fmt.Errorf("measured finding cites %s, which was refused", id)
				}
			}
		case ConfidenceInferred:
			for _, id := range finding.Evidence {
				if _, present := usable[id]; !present {
					return fmt.Errorf("finding cites %s, which is not among the sealed measurements", id)
				}
			}
		default:
			return errors.New("finding confidence must be measured or inferred")
		}
	}
	return nil
}

// validText accepts one line of prose: no control characters (a newline
// would let a claim start a Markdown heading in DESIGN.md), trimmed, bounded.
func validText(value string, limit int) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed != "" && trimmed == value && utf8.ValidString(value) && len(value) <= limit &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }) < 0
}

// Verification forms a design may promise.
const (
	VerificationWording     = "wording"
	VerificationMeasurement = "measurement"
)

// Verification says how the change will be judged after deployment: by the
// screen check (wording) or by re-running a probe against a threshold.
type Verification struct {
	Form string `json:"form"`
	// wording
	Path         string `json:"path,omitempty"`
	ExpectedText string `json:"expected_text,omitempty"`
	AbsentText   string `json:"absent_text,omitempty"`
	// measurement
	Probe     string            `json:"probe,omitempty"`
	Args      map[string]string `json:"args,omitempty"`
	Metric    string            `json:"metric,omitempty"`
	Threshold float64           `json:"threshold,omitempty"`
}

// FileChange names one file the applier may touch and what changes there.
type FileChange struct {
	Path    string   `json:"path"`
	Changes []string `json:"changes"`
}

// ModelDesignOutput is what the model answers with in design mode.
type ModelDesignOutput struct {
	Cause         string       `json:"cause"`
	CauseEvidence []string     `json:"cause_evidence"`
	Approach      string       `json:"approach"`
	Alternatives  []string     `json:"alternatives"`
	Files         []FileChange `json:"files"`
	Verification  Verification `json:"verification"`
	BlastRadius   []string     `json:"blast_radius"`
	NotDoing      []string     `json:"not_doing"`
}

// Design is the sealed design of one round, bound to the same round's
// investigation.
type Design struct {
	SchemaVersion int `json:"schema_version"`
	Identity
	Round               int    `json:"round"`
	InvestigationSHA256 string `json:"investigation_sha256"`

	Cause         string       `json:"cause"`
	CauseEvidence []string     `json:"cause_evidence"`
	Approach      string       `json:"approach"`
	Alternatives  []string     `json:"alternatives"`
	Files         []FileChange `json:"files"`
	Verification  Verification `json:"verification"`
	BlastRadius   []string     `json:"blast_radius"`
	NotDoing      []string     `json:"not_doing"`

	DesignSHA256 string `json:"design_sha256"`
}

// Bounds are the consumer's limits the design must respect.
type Bounds struct {
	AllowedFilePrefixes []string
	MaxFiles            int
	// Catalog resolves a measurement-form verification's probe.
	Catalog probe.Catalog
	// RepoRoot is the baseline working copy, read to check that a wording
	// promise is not already true.
	RepoRoot string
}

// NewDesign validates the model's output against the round's investigation
// and the consumer's bounds, seals the record and returns it.
func NewDesign(identity Identity, round int, output ModelDesignOutput, investigation Investigation, bounds Bounds) (Design, error) {
	record := Design{
		SchemaVersion: SchemaVersion, Identity: identity, Round: round, InvestigationSHA256: investigation.InvestigationSHA256,
		Cause: output.Cause, CauseEvidence: append([]string(nil), output.CauseEvidence...),
		Approach: output.Approach, Alternatives: append([]string(nil), output.Alternatives...),
		Files: append([]FileChange(nil), output.Files...), Verification: output.Verification,
		BlastRadius: append([]string(nil), output.BlastRadius...), NotDoing: append([]string(nil), output.NotDoing...),
	}
	digest, err := designDigest(record)
	if err != nil {
		return Design{}, errors.New("design could not be sealed")
	}
	record.DesignSHA256 = digest
	if err := record.Validate(identity, investigation, bounds); err != nil {
		return Design{}, err
	}
	return record, nil
}

// Validate re-derives the binding to the investigation, the file set, the
// cause's evidence, the verification and the fingerprint.
func (d Design) Validate(identity Identity, investigation Investigation, bounds Bounds) error {
	if d.SchemaVersion != SchemaVersion || d.Identity != identity || d.Round != investigation.Round ||
		investigation.Identity != identity || d.InvestigationSHA256 != investigation.InvestigationSHA256 ||
		!sha256Pattern.MatchString(d.InvestigationSHA256) {
		return errors.New("design is not bound to this round's investigation")
	}
	// The investigation handed in must be the sealed one, not an edited copy
	// that kept the fingerprint: re-derive it like every other parent record.
	if digest, err := investigationDigest(investigation); err != nil || digest != investigation.InvestigationSHA256 {
		return errors.New("design's investigation does not verify")
	}
	if bounds.MaxFiles < 1 || bounds.MaxFiles > 64 {
		return errors.New("design bounds are invalid")
	}
	if err := identity.validate(); err != nil {
		return err
	}
	if !validText(d.Cause, maxLongText) || !validText(d.Approach, maxLongText) {
		return errors.New("design cause or approach is invalid")
	}
	measured := investigation.MeasuredEvidence()
	if len(d.CauseEvidence) == 0 || len(d.CauseEvidence) > maxCauseEvidence {
		return errors.New("design cause cites no measurement")
	}
	for _, id := range d.CauseEvidence {
		if !measured[id] {
			return fmt.Errorf("design cause cites %s, which no measured finding of the investigation carries", id)
		}
	}
	if len(d.Alternatives) < 1 || len(d.Alternatives) > maxAlternatives || len(d.BlastRadius) == 0 || len(d.BlastRadius) > maxBlastRadius || len(d.NotDoing) > maxNotDoing {
		return errors.New("design lists are out of bounds (one to three alternatives, at least one blast radius item)")
	}
	for _, list := range [][]string{d.Alternatives, d.BlastRadius, d.NotDoing} {
		for _, item := range list {
			if !validText(item, maxShortText) {
				return errors.New("design list item is invalid")
			}
		}
	}
	if err := validateFiles(d.Files, bounds); err != nil {
		return err
	}
	if err := validateVerification(d.Verification, d.Files, bounds); err != nil {
		return err
	}
	digest, err := designDigest(d)
	if err != nil || digest != d.DesignSHA256 {
		return errors.New("design digest is invalid")
	}
	return nil
}

// FilePaths returns the design's file set, sorted, for the seal and the
// publish gate to check the candidate against.
func (d Design) FilePaths() []string {
	out := make([]string, 0, len(d.Files))
	for _, file := range d.Files {
		out = append(out, file.Path)
	}
	sort.Strings(out)
	return out
}

func validateFiles(files []FileChange, bounds Bounds) error {
	if len(files) == 0 || (bounds.MaxFiles > 0 && len(files) > bounds.MaxFiles) {
		return errors.New("design names no files or too many")
	}
	seen := map[string]bool{}
	for _, file := range files {
		if !relativePathPattern.MatchString(file.Path) || strings.Contains(file.Path, "..") || strings.Contains(file.Path, "//") ||
			filepath.Clean(file.Path) != file.Path || seen[file.Path] {
			return fmt.Errorf("design file path %q is invalid", file.Path)
		}
		seen[file.Path] = true
		if !withinPrefixes(file.Path, bounds.AllowedFilePrefixes) {
			return fmt.Errorf("design file %q is outside the allowed prefixes", file.Path)
		}
		if len(file.Changes) == 0 || len(file.Changes) > maxChangeNotes {
			return fmt.Errorf("design file %q lists no changes or too many", file.Path)
		}
		for _, change := range file.Changes {
			if !validText(change, maxShortText) {
				return fmt.Errorf("design file %q has an invalid change note", file.Path)
			}
		}
	}
	return nil
}

func withinPrefixes(path string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return false
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func validateVerification(v Verification, files []FileChange, bounds Bounds) error {
	switch v.Form {
	case VerificationWording:
		if v.Probe != "" || len(v.Args) > 0 || v.Metric != "" || v.Threshold != 0 {
			return errors.New("wording verification carries measurement fields")
		}
		if !strings.HasPrefix(v.Path, "/") || !validText(v.Path, maxShortText) || !validText(v.ExpectedText, maxShortText) ||
			utf8.RuneCountInString(v.ExpectedText) < 2 || (v.AbsentText != "" && !validText(v.AbsentText, maxShortText)) {
			return errors.New("wording verification is invalid")
		}
		if bounds.RepoRoot == "" {
			return errors.New("wording verification needs the baseline working copy to check against")
		}
		return wordingCheck(v, files, bounds.RepoRoot)
	case VerificationMeasurement:
		if v.Path != "" || v.ExpectedText != "" || v.AbsentText != "" {
			return errors.New("measurement verification carries wording fields")
		}
		spec, ok := bounds.Catalog.Lookup(v.Probe)
		if !ok {
			return fmt.Errorf("verification probe %q is not in the catalogue", v.Probe)
		}
		if _, refusal := bounds.Catalog.Resolve(probe.Request{Probe: v.Probe, Args: v.Args}); refusal != nil {
			return fmt.Errorf("verification probe request is refused: %s", refusal.Reason)
		}
		switch v.Metric {
		case "time_total", "status", "bytes", "rows", "value":
		default:
			return errors.New("verification metric is unknown")
		}
		if spec.Kind == probe.KindHTTP && (v.Metric == "rows" || v.Metric == "value") {
			return errors.New("verification metric does not fit an http probe")
		}
		if v.Threshold <= 0 {
			return errors.New("verification threshold must be positive")
		}
		return nil
	default:
		return errors.New("verification form must be wording or measurement")
	}
}

// wordingCheck applies the intake's own rule to the design's files as they
// stand at the baseline: the promised wording must not be there yet, and
// the wording promised to disappear must be present somewhere.
func wordingCheck(v Verification, files []FileChange, root string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("baseline working copy: %w", err)
	}
	absentFound := v.AbsentText == ""
	for _, file := range files {
		full := filepath.Join(root, file.Path)
		info, err := os.Lstat(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // a file the change creates
			}
			return fmt.Errorf("design file %q: %w", file.Path, err)
		}
		if !info.Mode().IsRegular() {
			// A symbolic link would let the check read outside the working
			// copy and report what it found through the objection.
			return fmt.Errorf("design file %q is not a regular file", file.Path)
		}
		if resolved, err := filepath.EvalSymlinks(full); err != nil || !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
			return fmt.Errorf("design file %q leaves the working copy", file.Path)
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("design file %q: %w", file.Path, err)
		}
		if strings.Contains(string(content), v.ExpectedText) {
			return fmt.Errorf("design file %q already contains the promised wording", file.Path)
		}
		if v.AbsentText != "" && strings.Contains(string(content), v.AbsentText) {
			absentFound = true
		}
	}
	if !absentFound {
		return errors.New("no design file contains the wording promised to disappear")
	}
	return nil
}

func investigationDigest(record Investigation) (string, error) {
	record.InvestigationSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func designDigest(record Design) (string, error) {
	record.DesignSHA256 = ""
	encoded, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return digestBytes(encoded), nil
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// DecodeModelInvestigationOutput and DecodeModelDesignOutput read the
// model's answers strictly: unknown fields are an error, like every other
// model answer the kernel accepts.
func DecodeModelInvestigationOutput(encoded []byte) (ModelInvestigationOutput, error) {
	var output ModelInvestigationOutput
	return output, decodeStrict(encoded, &output)
}

func DecodeModelDesignOutput(encoded []byte) (ModelDesignOutput, error) {
	var output ModelDesignOutput
	return output, decodeStrict(encoded, &output)
}

func decodeStrict(encoded []byte, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("trailing content after the JSON object")
	}
	return nil
}

// ReadInvestigation and ReadDesign load sealed records from disk without
// validating them; callers validate against their own inputs.
func ReadInvestigation(path string) (Investigation, error) {
	var record Investigation
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Investigation{}, err
	}
	return record, decodeStrict(encoded, &record)
}

func ReadDesign(path string) (Design, error) {
	var record Design
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Design{}, err
	}
	return record, decodeStrict(encoded, &record)
}

// Write stores a sealed record as indented JSON, owner-readable.
func Write(path string, record any) error {
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}
