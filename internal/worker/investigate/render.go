package investigate

import (
	"fmt"
	"strings"
)

// RenderDesign writes the human-readable copy of a sealed design. It is a
// pure function of the two records: the same input always yields the same
// text, so the applier's instruction and the reviewed record cannot drift
// apart. The model never writes this file.
func RenderDesign(design Design, investigation Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Design — round %d\n\n", design.Round)
	fmt.Fprintf(&b, "Design `%s` on investigation `%s` (measurements: %d lines, chain `%s`).\n\n",
		short(design.DesignSHA256), short(design.InvestigationSHA256), investigation.MeasurementsCount, short(investigation.MeasurementsChainSHA256))

	b.WriteString("## Cause\n\n")
	fmt.Fprintf(&b, "%s\n\nEvidence: %s\n\n", design.Cause, strings.Join(design.CauseEvidence, ", "))

	b.WriteString("## Approach\n\n")
	b.WriteString(design.Approach + "\n\n")
	if len(design.Alternatives) > 0 {
		b.WriteString("Not taken:\n\n")
		for _, alternative := range design.Alternatives {
			fmt.Fprintf(&b, "- %s\n", alternative)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Files\n\n")
	b.WriteString("Only these files change. Anything else is outside the design.\n\n")
	for _, file := range design.Files {
		fmt.Fprintf(&b, "### `%s`\n\n", file.Path)
		for _, change := range file.Changes {
			fmt.Fprintf(&b, "- %s\n", change)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Verification\n\n")
	switch design.Verification.Form {
	case VerificationWording:
		fmt.Fprintf(&b, "Screen check on `%s`: the page must show %q", design.Verification.Path, design.Verification.ExpectedText)
		if design.Verification.AbsentText != "" {
			fmt.Fprintf(&b, " and must no longer show %q", design.Verification.AbsentText)
		}
		b.WriteString(".\n\n")
	case VerificationMeasurement:
		fmt.Fprintf(&b, "Measurement: probe `%s`", design.Verification.Probe)
		if len(design.Verification.Args) > 0 {
			keys := make([]string, 0, len(design.Verification.Args))
			for key := range design.Verification.Args {
				keys = append(keys, key)
			}
			sortStrings(keys)
			parts := make([]string, 0, len(keys))
			for _, key := range keys {
				parts = append(parts, fmt.Sprintf("%s=%s", key, design.Verification.Args[key]))
			}
			fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
		}
		fmt.Fprintf(&b, ", %s ≤ %g after deployment.\n\n", design.Verification.Metric, design.Verification.Threshold)
	}

	b.WriteString("## Blast radius\n\n")
	for _, item := range design.BlastRadius {
		fmt.Fprintf(&b, "- %s\n", item)
	}
	b.WriteString("\n")
	if len(design.NotDoing) > 0 {
		b.WriteString("## Not doing\n\n")
		for _, item := range design.NotDoing {
			fmt.Fprintf(&b, "- %s\n", item)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Investigation summary\n\n")
	for _, finding := range investigation.Findings {
		marker := "inferred"
		if finding.Confidence == ConfidenceMeasured {
			marker = "measured: " + strings.Join(finding.Evidence, ", ")
		}
		fmt.Fprintf(&b, "- %s (%s)\n", finding.Claim, marker)
	}
	if len(investigation.Unknowns) > 0 {
		b.WriteString("\nUnknown:\n\n")
		for _, unknown := range investigation.Unknowns {
			fmt.Fprintf(&b, "- %s\n", unknown)
		}
	}
	b.WriteString("\n---\n")
	fmt.Fprintf(&b, "design_sha256: %s\ninvestigation_sha256: %s\n", design.DesignSHA256, design.InvestigationSHA256)
	return b.String()
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
