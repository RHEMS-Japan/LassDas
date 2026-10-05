package main

import (
	"strings"
	"testing"
)

// These are instructions to existing roles, not a test of a model's judgment.
func TestOrderedExampleAssignsKnowledgeWritebackWithoutANewStage(t *testing.T) {
	cfg := stagesExample(t)
	for _, phrase := range []string{
		"approved in-repository write destination during elicitation",
		"a knowledge reading location alone is not permission to write there",
		"file choices once, together with the necessary question",
		"With no requester answer, do not ask for a destination",
		"preserve existing knowledge, avoid duplicates on retries",
		"do not record credentials, unnecessary private information or guesses as answers",
		"The review role checks the written knowledge against the actual question and answer",
		"same reviewed pull request, never a separate unreviewed push",
	} {
		if !strings.Contains(cfg.Instructions, phrase) {
			t.Errorf("knowledge responsibility missing: %q", phrase)
		}
	}
	for _, role := range cfg.Roles {
		if role.Name == "work" && (len(role.Processes) != 1 || role.Processes[0].TrackerAccess != "read") {
			t.Fatal("work cannot read the assigned issue's actual questions and answers")
		}
	}
}
