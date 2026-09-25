package worker

import (
	"strings"
	"testing"
	"time"
)

// gapPlan is one destination's release path as the attendant seals it
// before the reception runs: something the round will build, and something
// the engine was handed no means for.
func gapPlan(t *testing.T, unapplied bool) *ReleasePathPlan {
	t.Helper()
	plan := ReleasePathPlan{
		SchemaVersion: ReleasePathSchemaVersion, Repository: "example/consumer",
		Configured: "production", DecidedAt: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
		Items: []ReleasePathItem{{
			Name: "反映を画面から確かめる入口", Kind: ReleasePathObservation,
			Detail: "画面から確かめられる入口を用意してください。",
		}},
	}
	if unapplied {
		plan.Items = append(plan.Items, ReleasePathItem{
			Name: ".github/workflows/deploy-staging.yml", Kind: ReleasePathWorkflow,
			Detail: "staging へ反映する workflow がリポジトリにありません。",
			Means:  "先頭がドットのディレクトリの中は本体が書けません。",
		})
	}
	if err := plan.Seal(); err != nil {
		t.Fatal(err)
	}
	return &plan
}

// The reception is the only place anything asks the requester anything, so
// a means this engine was not handed has to reach it there or nowhere. It
// arrives as the settings key and the sentence saying why the engine cannot
// write it itself.
func TestTheReceptionIsToldAboutAMeansItWasNotHanded(t *testing.T) {
	config, request, source := validArtifactFixture(t)

	prompt, err := readinessPrompt(source, request, config, nil, nil, nil, nil, gapPlan(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `"missing_means"`) ||
		!strings.Contains(prompt, `.github/workflows/deploy-staging.yml`) {
		t.Fatalf("the reception was not told what the engine is missing: %s", prompt)
	}
	// The checker sees the same thing, so a question about it reads as one
	// the rules asked for rather than one nothing accounts for.
	assessment, _ := testAssessmentPair(t, 1, testReadyOutput(), "pass", source, request, config)
	checkPrompt, err := readinessCheckPrompt(assessment, source, request, config, nil, nil, gapPlan(t, true))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(checkPrompt, `"missing_means"`) {
		t.Fatalf("the reception's checker was not told what the engine is missing: %s", checkPrompt)
	}
	// One question, in the round the reception already asks in, and never a
	// question when there is nothing missing.
	system := readinessSystemPrompt(defaultTestPolicy())
	for _, must := range []string{
		"USER_DATA_JSON.missing_means",
		"ask a single question covering the missing means",
		"Ask it once, with everything else, in this set",
		"do not ask it at all when missing_means is absent",
	} {
		if !strings.Contains(system, must) {
			t.Fatalf("the reception's rules do not say %q", must)
		}
	}
	// Release machinery inside the destination's own repository stopped
	// being a reason to refuse a ticket the moment the engine started
	// building it.
	if !strings.Contains(system, "Deployment machinery inside the destination's own repository is NOT out of scope") {
		t.Fatalf("the reception still refuses a ticket for needing a release path: %s", system)
	}
}

// A part the engine builds itself is not a means anybody is asked for, and
// a destination whose path is complete has no plan at all.
func TestTheReceptionIsAskedNothingWhenNothingIsMissing(t *testing.T) {
	config, request, source := validArtifactFixture(t)

	for name, plan := range map[string]*ReleasePathPlan{
		"a plan the engine can build in full": gapPlan(t, false),
		"no plan at all":                      nil,
	} {
		prompt, err := readinessPrompt(source, request, config, nil, nil, nil, nil, plan)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(prompt, `"missing_means"`) {
			t.Fatalf("%s put a question to the requester: %s", name, prompt)
		}
	}
}
