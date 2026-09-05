package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// A candidate that applies an approved design is held to it twice: the seal
// refuses a change outside the design's files, and the publish gate refuses
// a candidate whose design fingerprint, decision or file set do not agree
// (docs/INVESTIGATING_DESIGNER.md §7). Neither check trusts an AI.

// ChangedFilesWithinDesign refuses any changed path the design does not name.
func ChangedFilesWithinDesign(changed []string, design investigate.Design) error {
	allowed := map[string]bool{}
	for _, path := range design.FilePaths() {
		allowed[path] = true
	}
	var outside []string
	for _, path := range changed {
		if !allowed[path] {
			outside = append(outside, path)
		}
	}
	if len(outside) > 0 {
		sort.Strings(outside)
		return fmt.Errorf("the change touches files outside the design: %s", strings.Join(outside, ", "))
	}
	return nil
}

// DesignDecisionSummary is the part of a sealed design decision the gate
// reads: what was decided about which record.
type DesignDecisionSummary struct {
	Subject       string `json:"subject"`
	SubjectSHA256 string `json:"subject_sha256"`
	Outcome       string `json:"outcome"`
}

// ReadDesignDecisionSummary reads the decision's subject and outcome. The
// decision's own fingerprint and identity are validated where it is sealed;
// here the gate needs to know that this design was approved.
func ReadDesignDecisionSummary(path string) (DesignDecisionSummary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return DesignDecisionSummary{}, errors.New("design decision could not be read")
	}
	var summary DesignDecisionSummary
	if err := json.Unmarshal(raw, &summary); err != nil {
		return DesignDecisionSummary{}, errors.New("design decision is not valid JSON")
	}
	return summary, nil
}

// ValidateDesignBinding is the publish gate's check for a design-backed
// candidate: the candidate names the design, the design is intact, the
// decision approved that very design, and every candidate file is one the
// design lists.
func ValidateDesignBinding(candidate Candidate, design investigate.Design, decision DesignDecisionSummary) error {
	if !sha256Pattern.MatchString(candidate.DesignSHA256) {
		return errors.New("candidate carries no design fingerprint")
	}
	if !design.DigestMatches() || design.DesignSHA256 != candidate.DesignSHA256 {
		return errors.New("candidate and design fingerprints do not agree")
	}
	if design.DeliveryID != candidate.DeliveryID || design.InputSHA256 != candidate.InputSHA256 ||
		design.ConfigSHA256 != candidate.ConfigSHA256 || design.ToolSHA != candidate.ToolSHA || design.BaseSHA != candidate.BaseSHA {
		return errors.New("design belongs to another run")
	}
	if decision.Subject != "design" || decision.SubjectSHA256 != design.DesignSHA256 || decision.Outcome != "approved" {
		return errors.New("design decision did not approve this design")
	}
	changed := make([]string, 0, len(candidate.Files))
	for _, file := range candidate.Files {
		changed = append(changed, file.Path)
	}
	return ChangedFilesWithinDesign(changed, design)
}
