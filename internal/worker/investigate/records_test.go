package investigate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"automation.internal/ticket-ingress/internal/probe"
)

var testIdentity = Identity{
	DeliveryID:   "delivery-1",
	InputSHA256:  strings.Repeat("1", 64),
	ConfigSHA256: strings.Repeat("2", 64),
	ToolSHA:      "tool-abc",
	BaseSHA:      strings.Repeat("a", 40),
}

// measurementsFile records three probes: two usable, one refused.
func measurementsFile(t *testing.T) (string, *probe.Session) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("first\nsecond\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, err := probe.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "measurements.jsonl")
	recorder, err := probe.OpenRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	session := &probe.Session{Catalog: catalog, Recorder: recorder, RepoRoot: root}
	for _, request := range []probe.Request{
		{Probe: "repo.list"},
		{Probe: "repo.read", Args: map[string]string{"path": "notes.txt"}},
		{Probe: "nope"},
	} {
		if _, err := session.Run(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	return path, session
}

func goodInvestigation(t *testing.T, path string) Investigation {
	t.Helper()
	record, err := NewInvestigation(testIdentity, 1, ModelInvestigationOutput{
		Questions: []string{"Which file carries the wording?"},
		Findings: []Finding{
			{Claim: "The wording lives in notes.txt", Evidence: []string{"m-0002"}, Confidence: ConfidenceMeasured},
			{Claim: "Nothing else references it", Confidence: ConfidenceInferred},
		},
		Unknowns: []string{"Whether the page caches the text"},
		Next:     "Change the second line.",
	}, path, 3, Budget{ProbesUsed: 3, ElapsedSeconds: 42})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestInvestigationRequiresMeasuredEvidence(t *testing.T) {
	path, session := measurementsFile(t)
	record := goodInvestigation(t, path)
	if record.MeasurementsCount != 3 || record.MeasurementsChainSHA256 != session.Recorder.Chain() || record.InvestigationSHA256 == "" {
		t.Fatalf("sealed record: %+v", record)
	}
	if err := record.Validate(testIdentity, path); err != nil {
		t.Fatalf("re-validation: %v", err)
	}
	base := ModelInvestigationOutput{Questions: []string{"q"}, Next: "n", Findings: []Finding{{Claim: "c", Confidence: ConfidenceMeasured, Evidence: []string{"m-0001"}}}}
	refused := []struct {
		name   string
		mutate func(*ModelInvestigationOutput)
		reason string
	}{
		{"measured without evidence", func(o *ModelInvestigationOutput) { o.Findings[0].Evidence = nil }, "cites no measurement"},
		{"measured citing a refusal", func(o *ModelInvestigationOutput) { o.Findings[0].Evidence = []string{"m-0003"} }, "was refused"},
		{"measured citing beyond the prefix", func(o *ModelInvestigationOutput) { o.Findings[0].Evidence = []string{"m-0004"} }, "not among the sealed"},
		{"malformed id", func(o *ModelInvestigationOutput) { o.Findings[0].Evidence = []string{"measurement 1"} }, "malformed"},
		{"unknown confidence", func(o *ModelInvestigationOutput) { o.Findings[0].Confidence = "certain" }, "measured or inferred"},
		{"no questions", func(o *ModelInvestigationOutput) { o.Questions = nil }, "wrong number"},
		{"empty next", func(o *ModelInvestigationOutput) { o.Next = "  " }, "next step"},
	}
	for _, tc := range refused {
		output := base
		output.Findings = []Finding{base.Findings[0]}
		tc.mutate(&output)
		if _, err := NewInvestigation(testIdentity, 1, output, path, 3, Budget{ProbesUsed: 3}); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	// A report sealed over the first two lines stays valid after a third round appends.
	two, err := NewInvestigation(testIdentity, 1, ModelInvestigationOutput{Questions: []string{"q"}, Next: "n",
		Findings: []Finding{{Claim: "c", Confidence: ConfidenceMeasured, Evidence: []string{"m-0002"}}}}, path, 2, Budget{ProbesUsed: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Run(context.Background(), probe.Request{Probe: "repo.list"}); err != nil {
		t.Fatal(err)
	}
	if err := two.Validate(testIdentity, path); err != nil {
		t.Errorf("prefix report after append: %v", err)
	}
	// Tampering with the sealed text is caught.
	tampered := record
	tampered.Next = "Change the first line."
	if err := tampered.Validate(testIdentity, path); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("tampered record: %v", err)
	}
	if err := record.Validate(Identity{DeliveryID: "other"}, path); err == nil {
		t.Error("record accepted under another identity")
	}
	if _, err := NewInvestigation(testIdentity, 1, base, path, 2, Budget{ProbesUsed: 1}); err == nil {
		t.Error("probes_used below measurements_count accepted")
	}
}

func testBounds(t *testing.T, root string) Bounds {
	t.Helper()
	catalog, err := probe.NewCatalog([]probe.Spec{{ID: "http.timing", Kind: probe.KindHTTP, Hosts: []string{"app-stg.example.invalid"},
		Methods: []string{"GET"}, Args: map[string]string{"path": `/[a-z/]{0,40}`}}})
	if err != nil {
		t.Fatal(err)
	}
	return Bounds{AllowedFilePrefixes: []string{"web/", "docs/"}, MaxFiles: 4, Catalog: catalog, RepoRoot: root}
}

func goodDesignOutput() ModelDesignOutput {
	return ModelDesignOutput{
		Cause:         "The label is hard-coded in the template",
		CauseEvidence: []string{"m-0002"},
		Approach:      "Replace the label in the template",
		Alternatives:  []string{"Add a translation key"},
		Files:         []FileChange{{Path: "web/page.tmpl", Changes: []string{"replace the label"}}},
		Verification:  Verification{Form: VerificationWording, Path: "/page", ExpectedText: "New label", AbsentText: "Old label"},
		BlastRadius:   []string{"the page header"},
		NotDoing:      []string{"renaming the route"},
	}
}

func TestDesignValidation(t *testing.T) {
	path, _ := measurementsFile(t)
	investigation := goodInvestigation(t, path)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "page.tmpl"), []byte("<h1>Old label</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bounds := testBounds(t, root)
	design, err := NewDesign(testIdentity, 1, goodDesignOutput(), investigation, bounds)
	if err != nil {
		t.Fatalf("good design refused: %v", err)
	}
	if err := design.Validate(testIdentity, investigation, bounds); err != nil {
		t.Fatalf("re-validation: %v", err)
	}
	if got := design.FilePaths(); len(got) != 1 || got[0] != "web/page.tmpl" {
		t.Errorf("FilePaths = %v", got)
	}
	refused := []struct {
		name   string
		mutate func(*ModelDesignOutput)
		reason string
	}{
		{"cause without measured evidence", func(o *ModelDesignOutput) { o.CauseEvidence = nil }, "cites no measurement"},
		{"cause citing an inferred-only id", func(o *ModelDesignOutput) { o.CauseEvidence = []string{"m-0001"} }, "no measured finding"},
		{"file outside the prefixes", func(o *ModelDesignOutput) { o.Files[0].Path = "internal/secret.go" }, "outside the allowed prefixes"},
		{"file traversal", func(o *ModelDesignOutput) { o.Files[0].Path = "web/../internal/x.go" }, "invalid"},
		{"too many files", func(o *ModelDesignOutput) {
			o.Files = []FileChange{{Path: "web/a", Changes: []string{"x"}}, {Path: "web/b", Changes: []string{"x"}}, {Path: "web/c", Changes: []string{"x"}}, {Path: "web/d", Changes: []string{"x"}}, {Path: "web/e", Changes: []string{"x"}}}
		}, "too many"},
		{"no files", func(o *ModelDesignOutput) { o.Files = nil }, "no files"},
		{"wording already present", func(o *ModelDesignOutput) { o.Verification.ExpectedText = "Old label" }, "already contains"},
		{"absent text missing", func(o *ModelDesignOutput) { o.Verification.AbsentText = "Older label" }, "promised to disappear"},
		{"wording without path", func(o *ModelDesignOutput) { o.Verification.Path = "page" }, "wording verification is invalid"},
		{"unknown form", func(o *ModelDesignOutput) { o.Verification.Form = "manual" }, "wording or measurement"},
		{"measurement probe unknown", func(o *ModelDesignOutput) {
			o.Verification = Verification{Form: VerificationMeasurement, Probe: "http.other", Args: map[string]string{"path": "/page"}, Metric: "time_total", Threshold: 3}
		}, "not in the catalogue"},
		{"measurement request out of shape", func(o *ModelDesignOutput) {
			o.Verification = Verification{Form: VerificationMeasurement, Probe: "http.timing", Args: map[string]string{"path": "/page?x=1"}, Metric: "time_total", Threshold: 3}
		}, "refused"},
		{"measurement threshold zero", func(o *ModelDesignOutput) {
			o.Verification = Verification{Form: VerificationMeasurement, Probe: "http.timing", Args: map[string]string{"path": "/page"}, Metric: "time_total"}
		}, "threshold"},
		{"empty blast radius", func(o *ModelDesignOutput) { o.BlastRadius = nil }, "out of bounds"},
	}
	for _, tc := range refused {
		output := goodDesignOutput()
		tc.mutate(&output)
		if _, err := NewDesign(testIdentity, 1, output, investigation, bounds); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	measurementForm := goodDesignOutput()
	measurementForm.Verification = Verification{Form: VerificationMeasurement, Probe: "http.timing", Args: map[string]string{"path": "/page"}, Metric: "time_total", Threshold: 3}
	if _, err := NewDesign(testIdentity, 1, measurementForm, investigation, bounds); err != nil {
		t.Errorf("measurement form refused: %v", err)
	}
	// A design bound to another round's investigation is refused.
	other := investigation
	other.Round = 2
	if err := design.Validate(testIdentity, other, bounds); err == nil {
		t.Error("design accepted against another round")
	}
	tampered := design
	tampered.Approach = "Do something else"
	if err := tampered.Validate(testIdentity, investigation, bounds); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("tampered design: %v", err)
	}
}

func TestDesignRenderingIsDeterministic(t *testing.T) {
	path, _ := measurementsFile(t)
	investigation := goodInvestigation(t, path)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "web", "page.tmpl"), []byte("<h1>Old label</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := goodDesignOutput()
	output.Verification = Verification{Form: VerificationMeasurement, Probe: "http.timing", Args: map[string]string{"path": "/page", "method": "GET"}, Metric: "time_total", Threshold: 3}
	design, err := NewDesign(testIdentity, 1, output, investigation, testBounds(t, root))
	if err != nil {
		t.Fatal(err)
	}
	first := RenderDesign(design, investigation)
	for i := 0; i < 20; i++ {
		if RenderDesign(design, investigation) != first {
			t.Fatal("rendering differs between calls")
		}
	}
	for _, want := range []string{"# Design — round 1", "## Cause", "m-0002", "web/page.tmpl", "replace the label", "method=GET, path=/page", "time_total ≤ 3", "the page header", "renaming the route", "design_sha256: " + design.DesignSHA256, "measured: m-0002"} {
		if !strings.Contains(first, want) {
			t.Errorf("rendering lacks %q", want)
		}
	}
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "design-1.json"), design); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDesign(filepath.Join(dir, "design-1.json"))
	if err != nil || read.DesignSHA256 != design.DesignSHA256 || RenderDesign(read, investigation) != first {
		t.Errorf("round trip: %v", err)
	}
	if _, err := DecodeModelDesignOutput([]byte(`{"cause":"x","extra":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
}
