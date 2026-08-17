package worker

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"automation.internal/ticket-ingress/internal/hook"
)

func validTicketEnvelope(t *testing.T, description string) hook.DispatchEnvelope {
	t.Helper()
	snapshot := hook.TicketSnapshot{
		SchemaVersion: hook.SnapshotSchemaVersion,
		SpaceKey:      "example", ActivityID: 1, ActivityType: 1, ProjectID: 909057, ProjectKey: "TICKET",
		IssueID: 2, IssueKey: "TICKET-3", IssueKeyID: 3, CreatorID: 9903853,
		RunID: "run_20260802_alpha", CreatedAt: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC),
		Target:    hook.DeliveryTarget{RepositoryID: 101, WorkflowRefSHA256: strings.Repeat("a", 64)},
		Untrusted: hook.UntrustedTicketData{Summary: "Change one visible label", Description: description},
	}
	envelope, err := hook.SealSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func validTicketDescription() string {
	return strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Target-File: client/src/components/Example.tsx",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		"Replace the visible label while preserving the surrounding behavior.",
	}, "\n")
}

func TestParseTicket(t *testing.T) {
	request, err := ParseTicket(validTicketEnvelope(t, validTicketDescription()), validTestConfig())
	if err != nil {
		t.Fatalf("ParseTicket() error = %v", err)
	}
	if request.IssueKey != "TICKET-3" || request.VerificationPath != "/settings" || len(request.TargetFiles) != 1 {
		t.Fatalf("request = %+v", request)
	}
}

func TestParseTicketBindsConfigAndToolRevision(t *testing.T) {
	config := validTestConfig()
	toolSHA := strings.Repeat("c", 40)
	request, err := ParseTicketWithToolSHA(validTicketEnvelope(t, validTicketDescription()), config, toolSHA)
	if err != nil {
		t.Fatal(err)
	}
	configSHA, err := config.SHA256()
	if err != nil || request.ConfigSHA256 != configSHA || request.ToolSHA != toolSHA {
		t.Fatalf("request identity = %+v; error = %v", request, err)
	}
	changed := config
	changed.Consumers[0].Mode.MaxChangedBytes--
	if err := request.Validate(changed); err == nil {
		t.Fatal("TicketRequest.Validate() accepted config drift")
	}
}

func TestParseTicketSortsTargetFiles(t *testing.T) {
	description := strings.Replace(validTicketDescription(),
		"Target-File: client/src/components/Example.tsx",
		"Target-File: client/src/z.tsx\nTarget-File: client/src/a.tsx", 1)
	request, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(request.TargetFiles, ",") != "client/src/a.tsx,client/src/z.tsx" {
		t.Fatalf("target files = %v", request.TargetFiles)
	}
}

func TestParseTicketRejectsConfigurationInjection(t *testing.T) {
	tests := map[string]string{
		"repository": strings.Replace(validTicketDescription(), "Automation-Mode:", "Repository: attacker/repo\nAutomation-Mode:", 1),
		"workflow":   strings.Replace(validTicketDescription(), "Verification-Path:", "Workflow: attacker.yml\nVerification-Path:", 1),
		"extra mode": strings.Replace(validTicketDescription(), "---", "Automation-Mode: another\n---", 1),
	}
	for name, description := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
				t.Fatal("ParseTicket() accepted an injected configuration header")
			}
		})
	}
}

func TestParseTicketRejectsUnsafeTargets(t *testing.T) {
	tests := []string{"../secret", "client/../../secret", "server/app.go", "client/src/", "client/src/has space.tsx", "client/src/a:option.tsx"}
	for _, filename := range tests {
		t.Run(filename, func(t *testing.T) {
			description := strings.Replace(validTicketDescription(), "client/src/components/Example.tsx", filename, 1)
			if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
				t.Fatal("ParseTicket() accepted an unsafe target")
			}
		})
	}
}

func TestParseTicketRejectsRunMismatchAndDuplicateHeader(t *testing.T) {
	tests := []string{
		strings.Replace(validTicketDescription(), "run_20260802_alpha", "run_20260802_other", 1),
		strings.Replace(validTicketDescription(), "---", "Automation-Run-ID: run_20260802_alpha\n---", 1),
	}
	for _, description := range tests {
		if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
			t.Fatal("ParseTicket() accepted an invalid run binding")
		}
	}
}

func TestParseTicketRejectsUnsafeVerificationPath(t *testing.T) {
	for _, value := range []string{"https://attacker.invalid", "//attacker.invalid", "/a/../b", "/search?q=x", "/%2e%2e/secret", "/has space", "/<script>"} {
		description := strings.Replace(validTicketDescription(), "/settings", value, 1)
		if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
			t.Errorf("ParseTicket() accepted verification path %q", value)
		}
	}
}

func TestParseTicketRejectsNonUniqueShortAcceptanceText(t *testing.T) {
	for _, replacement := range []string{
		"Expected-Text: x",
		"Expected-Text: Updated label\nAbsent-Text: old",
	} {
		description := validTicketDescription()
		if strings.HasPrefix(replacement, "Expected-Text: Updated") {
			description = strings.Replace(description, "Expected-Text: Updated label\nAbsent-Text: Old label", replacement, 1)
		} else {
			description = strings.Replace(description, "Expected-Text: Updated label", replacement, 1)
		}
		if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
			t.Fatalf("ParseTicket() accepted short acceptance text in %q", replacement)
		}
	}
}

func TestParseTicketRequiresAbsentText(t *testing.T) {
	description := strings.Replace(validTicketDescription(), "Absent-Text: Old label\n", "", 1)
	if _, err := ParseTicket(validTicketEnvelope(t, description), validTestConfig()); err == nil {
		t.Fatal("ParseTicket() accepted a ticket without Absent-Text")
	}
}

// draftTicketDescription is what a requester can write without having read the
// repository: what should change, what it becomes, and where to see it. Which
// file implements it is deliberately absent.
func draftTicketDescription() string {
	return strings.Join([]string{
		"Automation-Run-ID: run_20260802_alpha",
		"Automation-Mode: client-visible-change",
		"Verification-Path: /settings",
		"Expected-Text: Updated label",
		"Absent-Text: Old label",
		"---",
		"Replace the visible label while preserving the surrounding behavior.",
	}, "\n")
}

func TestParseTicketDraftAcceptsATicketWithoutTargetFiles(t *testing.T) {
	config := validTestConfig()
	draft, files, err := ParseTicketDraft(validTicketEnvelope(t, draftTicketDescription()), config, unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("ParseTicketDraft() error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none", files)
	}
	if draft.VerificationPath != "/settings" || draft.ExpectedText != "Updated label" || draft.AbsentText != "Old label" {
		t.Fatalf("draft = %+v", draft)
	}

	// Completing the draft must produce exactly the contract the requester
	// would have got by naming the file themselves: the two entry paths may
	// never diverge, or the derived route would run under a different
	// contract than the written one.
	completed, err := draft.WithTargetFiles([]string{"client/src/components/Example.tsx"}, config)
	if err != nil {
		t.Fatalf("WithTargetFiles() error = %v", err)
	}
	written, err := ParseTicketWithToolSHA(validTicketEnvelope(t, validTicketDescription()), config, unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("ParseTicketWithToolSHA() error = %v", err)
	}
	// The two tickets carry different text, so their sealed identities differ
	// by design; what must not differ is the work the contract describes.
	completed.DeliveryID, completed.InputSHA256 = written.DeliveryID, written.InputSHA256
	if !reflect.DeepEqual(completed, written) {
		t.Fatalf("derived contract differs from the written one:\n derived = %+v\n written = %+v", completed, written)
	}
}

func TestParseTicketStillRequiresTargetFilesOnTheWrittenPath(t *testing.T) {
	// The existing entry point keeps its contract, so a run that measures the
	// written path cannot silently fall through to derivation.
	if _, err := ParseTicketWithToolSHA(validTicketEnvelope(t, draftTicketDescription()), validTestConfig(), unboundDevelopmentToolSHA); err == nil {
		t.Fatal("a ticket without target files was accepted on the written path")
	}
}

func TestDraftCompletionEnforcesTheWriteScope(t *testing.T) {
	config := validTestConfig()
	draft, _, err := ParseTicketDraft(validTicketEnvelope(t, draftTicketDescription()), config, unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("ParseTicketDraft() error = %v", err)
	}
	for _, run := range []struct {
		name  string
		files []string
	}{
		{name: "no files", files: nil},
		{name: "outside the allowed prefix", files: []string{"server/src/main.go"}},
		{name: "escaping the tree", files: []string{"client/src/../../etc/passwd"}},
		{name: "beyond the file budget", files: []string{"client/src/a.tsx", "client/src/b.tsx", "client/src/c.tsx", "client/src/d.tsx"}},
		{name: "duplicated", files: []string{"client/src/a.tsx", "client/src/a.tsx"}},
	} {
		t.Run(run.name, func(t *testing.T) {
			if _, err := draft.WithTargetFiles(run.files, config); err == nil {
				t.Fatalf("completion accepted %v", run.files)
			}
		})
	}
	// The caller may hand the files in any order; the contract is sorted.
	completed, err := draft.WithTargetFiles([]string{"client/src/b.tsx", "client/src/a.tsx"}, config)
	if err != nil {
		t.Fatalf("WithTargetFiles() error = %v", err)
	}
	if completed.TargetFiles[0] != "client/src/a.tsx" || completed.TargetFiles[1] != "client/src/b.tsx" {
		t.Fatalf("target files = %v, want sorted", completed.TargetFiles)
	}
}

// A ticket written in Backlog normally ends with a newline, and people put a
// blank line after the separator. Neither carries meaning, so neither may turn
// into "the input was not in the permitted format".
func TestParseTicketToleratesTheWhitespaceRealTicketsCarry(t *testing.T) {
	config := validTestConfig()
	base, err := ParseTicketWithToolSHA(validTicketEnvelope(t, validTicketDescription()), config, unboundDevelopmentToolSHA)
	if err != nil {
		t.Fatalf("baseline ticket rejected: %v", err)
	}
	for _, run := range []struct {
		name        string
		description string
	}{
		{name: "trailing newline", description: validTicketDescription() + "\n"},
		{name: "several trailing newlines", description: validTicketDescription() + "\n\n\n"},
		{name: "blank line after the separator", description: strings.Replace(validTicketDescription(), "---\n", "---\n\n", 1)},
		{name: "windows newline at the end", description: validTicketDescription() + "\r\n"},
		{name: "trailing spaces", description: validTicketDescription() + "   \n"},
	} {
		t.Run(run.name, func(t *testing.T) {
			request, err := ParseTicketWithToolSHA(validTicketEnvelope(t, run.description), config, unboundDevelopmentToolSHA)
			if err != nil {
				t.Fatalf("a normally written ticket was rejected: %v", err)
			}
			// Identity differs because the text differs; the work must not.
			request.DeliveryID, request.InputSHA256 = base.DeliveryID, base.InputSHA256
			if !reflect.DeepEqual(request, base) {
				t.Fatalf("whitespace changed the contract:\n got = %+v\n want = %+v", request, base)
			}
		})
	}
}
