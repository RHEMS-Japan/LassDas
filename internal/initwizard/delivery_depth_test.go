package initwizard

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	runtimeconfig "automation.internal/ticket-ingress/internal/runtime"
)

// Every question the interview asks has to be an answer the file may
// carry: a written answer whose id nothing asks for is refused outright,
// so an unregistered id makes the whole setup file unusable rather than
// just ignoring one line.
func TestEveryDeliveryDepthQuestionIsAnAnswerTheFileMayCarry(t *testing.T) {
	known := map[string]bool{}
	for _, requirement := range append(RequiredAnswers(), OptionalAnswers()...) {
		known[requirement.ID] = true
	}
	for _, id := range []string{
		"delivery-depth",
		"deliver-checks-profile", "deliver-integrate-profile", "deliver-promote-profile",
		"deliver-enabled-after", "staging-login-url", "production-login-url", "observation-language",
	} {
		if !known[id] {
			t.Fatalf("the interview asks %q and the answer file refuses it", id)
		}
	}
}

// A depth answered in the file is refused there when it is not one of the
// three. The list the interview offers answers with the wizard's own
// proposal, so a typo that reached the state would deliver something the
// file did not ask for without anything saying so.
func TestAnUnknownDepthIsRefusedWhereTheFileIsRead(t *testing.T) {
	for _, depth := range []string{"pull_request", "integration", "production"} {
		if err := CheckDeliveryDepth(depth); err != nil {
			t.Fatalf("%q was refused: %v", depth, err)
		}
	}
	for _, depth := range []string{"", "prod", "Production", "staging"} {
		if err := CheckDeliveryDepth(depth); err == nil {
			t.Fatalf("%q was accepted as a depth", depth)
		}
	}
}

// A depth deeper than the proposal does not reach the configuration of a
// destination that cannot carry it. The loader refuses a command-line
// destination that carries a delivery continuation at all, so writing the
// answers would produce an instance that does not start — a worse answer
// than a shallower delivery, and a silent one.
func TestADeeperDepthDoesNotBreakACommandLineInstance(t *testing.T) {
	s, secrets := wizardFixture(t)
	s.Delivery = "production"
	s.Deliver = runtimeconfig.DeliverConfig{ChecksProfile: "checks", IntegrateProfile: "integrate",
		PromoteProfile: "promote", EnabledAfter: "2026-09-25T00:00:00Z"}
	s.StagingLoginURL = "https://staging.example.test/login"
	s.ProductionLoginURL = "https://www.example.test/login"
	s.ObservationLanguage = "ja"

	consumer, runtime, _, err := Generate(s, secrets)
	if err != nil {
		t.Fatalf("the depth answers made the configuration invalid: %v", err)
	}
	// Read back the way the pod reads it, which is where the refusal would
	// land: a command-line destination carrying a delivery continuation is
	// refused by the loader, not by the generator.
	dir := t.TempDir()
	runtime.ConsumerConfigPath = filepath.Join(dir, "config", "m1-consumer.json")
	if err := writeConfigs(dir, consumer, runtime); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeconfig.Load(filepath.Join(dir, "config", "runtime.json")); err != nil {
		t.Fatalf("the generated instance does not load: %v", err)
	}
	if runtime.Chain.Deliver != (runtimeconfig.DeliverConfig{}) {
		t.Fatalf("a command-line destination was given a delivery continuation: %+v", runtime.Chain.Deliver)
	}
	if got := consumer.Consumers[0]; got.Delivery != "pull_request" ||
		got.StagingLoginURL != "" || got.ProductionLoginURL != "" || got.ObservationLanguage != "" {
		t.Fatalf("a command-line destination was given a screen: %+v", got)
	}
	// The answers are not lost: they stay in the journal for the day this
	// project has a screen.
	if s.Delivery != "production" || s.Deliver.ChecksProfile != "checks" {
		t.Fatalf("the answers were dropped from the journal: %+v", s)
	}
}

// The interview says, while the answer can still be changed, that this
// version will not carry the depth it just accepted.
func TestTheInterviewSaysACommandLineDestinationStopsAtTheProposal(t *testing.T) {
	s, _ := wizardFixture(t)
	said := []string{}
	wizard := &Wizard{UI: &depthUI{proposed: 2, answers: map[string]string{}, said: &said}}
	if err := wizard.deliveryDepth(s); err != nil {
		t.Fatalf("deliveryDepth: %v", err)
	}
	if s.Delivery != "production" {
		t.Fatalf("delivery = %q", s.Delivery)
	}
	if !strings.Contains(strings.Join(said, "\n"), "Pull Request までで止まります") {
		t.Fatalf("the interview did not say what this version does: %v", said)
	}
}

// depthUI answers the depth question the way a file-driven run does: the
// list takes the proposal, the plain questions take what the file has.
type depthUI struct {
	proposed int
	answers  map[string]string
	said     *[]string
}

func (u *depthUI) Ask(id, _, fallback string, _ bool) (string, error) {
	if value, ok := u.answers[id]; ok {
		return value, nil
	}
	return fallback, nil
}
func (u *depthUI) Choose(_, _ string, _ []Option, _ int) (int, error) { return u.proposed, nil }
func (u *depthUI) Confirm(string) (bool, error)                       { return true, nil }
func (u *depthUI) Info(value string)                                  { *u.said = append(*u.said, value) }

// depthAnswers is a setup file carrying the depth and everything that
// follows it, as an agent writes one.
func depthAnswers(t *testing.T, entries map[string]string) Answers {
	t.Helper()
	answers := Answers{Answers: map[string]json.RawMessage{}}
	for id, value := range entries {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		answers.Answers[id] = encoded
	}
	return answers
}

// A file carrying all eight is accepted. The reviewer's run got eight
// refusals of the form 「本体が聞かない質問への回答です」, which do not
// ignore one line: they make the whole setup file unusable.
func TestASetupFileCarryingEveryDepthAnswerIsAccepted(t *testing.T) {
	answers := depthAnswers(t, map[string]string{
		"delivery-depth":            "production",
		"deliver-checks-profile":    "lassdas-checks",
		"deliver-integrate-profile": "lassdas-integrate",
		"deliver-promote-profile":   "lassdas-promote",
		"deliver-enabled-after":     "2026-09-25T00:00:00Z",
		"staging-login-url":         "https://staging.example.test/login",
		"production-login-url":      "https://www.example.test/login",
		"observation-language":      "ja",
	})
	for _, problem := range answers.Check("") {
		if strings.Contains(problem, "本体が聞かない質問への回答です") {
			t.Fatalf("a question the interview asks was refused: %s", problem)
		}
	}
}

// The file decides the depth with nobody at the keyboard. The list answers
// with the wizard's own proposal, so the answer has to reach the state
// before the interview runs or the file is silently overruled by the
// default.
func TestAFileAnsweringProductionSetsTheDepthWithNoTerminal(t *testing.T) {
	s, _ := wizardFixture(t)
	answers := depthAnswers(t, map[string]string{
		"delivery-depth":            "production",
		"deliver-checks-profile":    "lassdas-checks",
		"deliver-integrate-profile": "lassdas-integrate",
		"deliver-promote-profile":   "lassdas-promote",
		"deliver-enabled-after":     "2026-09-25T00:00:00Z",
		"staging-login-url":         "https://staging.example.test/login",
		"production-login-url":      "https://www.example.test/login",
		"observation-language":      "ja",
	})
	if err := SeedDeliveryDepth(s, answers); err != nil {
		t.Fatalf("SeedDeliveryDepth: %v", err)
	}
	wizard := &Wizard{UI: &AnswersUI{Answers: answers, Project: "sample-cli"}}
	if err := wizard.deliveryDepth(s); err != nil {
		t.Fatalf("deliveryDepth with no terminal: %v", err)
	}
	if s.Delivery != "production" {
		t.Fatalf("delivery = %q, want production", s.Delivery)
	}
	if s.Deliver.ChecksProfile != "lassdas-checks" || s.Deliver.PromoteProfile != "lassdas-promote" ||
		s.Deliver.EnabledAfter != "2026-09-25T00:00:00Z" {
		t.Fatalf("the cards were not read from the file: %+v", s.Deliver)
	}
	if s.ProductionLoginURL != "https://www.example.test/login" || s.ObservationLanguage != "ja" {
		t.Fatalf("the screens were not read from the file: %+v", s)
	}
}

// A file that answers only the depth finishes. The seven that follow are
// blanks the engine then builds or names, so refusing the run for want of
// them would make the depth question unanswerable by a file at all.
func TestAFileAnsweringOnlyTheDepthStillFinishes(t *testing.T) {
	s, _ := wizardFixture(t)
	answers := depthAnswers(t, map[string]string{"delivery-depth": "production"})
	if err := SeedDeliveryDepth(s, answers); err != nil {
		t.Fatalf("SeedDeliveryDepth: %v", err)
	}
	wizard := &Wizard{UI: &AnswersUI{Answers: answers, Project: "sample-cli"}}
	if err := wizard.deliveryDepth(s); err != nil {
		t.Fatalf("a file answering only the depth did not finish: %v", err)
	}
	if s.Delivery != "production" || s.Deliver.ChecksProfile != "" || s.ProductionLoginURL != "" {
		t.Fatalf("blanks were not left blank: %q / %+v", s.Delivery, s.Deliver)
	}
}

// A depth the engine does not know is refused where the file is read.
func TestAFileWithAnUnknownDepthIsRefusedBeforeTheInterview(t *testing.T) {
	s, _ := wizardFixture(t)
	if err := SeedDeliveryDepth(s, depthAnswers(t, map[string]string{"delivery-depth": "prod"})); err == nil {
		t.Fatal("an unknown depth was seeded into the state")
	}
	if s.Delivery != "" {
		t.Fatalf("delivery = %q after a refusal", s.Delivery)
	}
}
