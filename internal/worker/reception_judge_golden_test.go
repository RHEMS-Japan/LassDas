package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

// A decision sealed by the engine before the reception had a judge still
// reads, still passes the gate, and still encodes to the bytes it was
// sealed as.
//
// Three JSON tags carry that, and all three are one word long. The settled
// points, the judgment and a question's proposed default are all omitted
// when empty, so a decision that has none encodes exactly as it did before
// the fields existed - and its digest, which is taken over those bytes, is
// the one sealed into it. Drop an omitempty from any of the three and every
// decision any older binary sealed fails the gate on "readiness decision
// digest is invalid", in flight, with nothing in the change that looks like
// it could have done it.
//
// The goldens below were produced by that earlier engine, not by this one:
// generated from this package's own fixtures at the commit this branch
// starts from, so they are what the previous binary really wrote rather
// than what this one says it would have written.
const (
	goldenReadyBeforeTheJudge = `{"schema_version":2,"delivery_id":"delivery_975c3a819df5f625999de6544254f0a5","input_sha256":"55b323bf6e049485937ce29540201d9356170b569d10d29e0785572cd5bcab0a","config_sha256":"e5c13b3b2705862c505a24fde4ffd72296b650b44200d366cea8961b7d31a076","tool_sha":"0000000000000000000000000000000000000000","source_sha256":"3a068b1f2ee5957036cf5885e0ed92a164c83478d3fd0975fde066ab8c8935fc","outcome":"ready","attempts":1,"assessment_sha256s":["7affe6bf23369b3c66bcd06b89dabff92e5fdc2e5f889a262a730cc0c71f62b9"],"check_sha256s":["4f6cc8bc805df42057dd58adc37fa7b7b67603fff57f32e8847e985955806bc3"],"questions":[],"request_kind":"change","needs_design":true,"design_reason":"approach_not_in_ticket","approach_in_ticket":false,"decision_sha256":"cde822cde2e40970e3f2d30c460a0fca210b7c3d36fc458f8b1a7fa88311f873"}`

	goldenClarificationBeforeTheJudge = `{"schema_version":2,"delivery_id":"delivery_975c3a819df5f625999de6544254f0a5","input_sha256":"55b323bf6e049485937ce29540201d9356170b569d10d29e0785572cd5bcab0a","config_sha256":"e5c13b3b2705862c505a24fde4ffd72296b650b44200d366cea8961b7d31a076","tool_sha":"0000000000000000000000000000000000000000","source_sha256":"3a068b1f2ee5957036cf5885e0ed92a164c83478d3fd0975fde066ab8c8935fc","outcome":"clarification_required","attempts":1,"assessment_sha256s":["388966fb526f7b10c7fcce29188f277f5104549ec3368bf4ad67c2d2949e7db9"],"check_sha256s":["0329dd44f2f4ac181db64c2242e040a7c9a9e79a9a0563c788a609778c93e5cc"],"questions":[{"id":"Q1","dimension":"user_visible_behavior","question":"Should the label change on both language screens?","why_blocking":"The choice changes which screens the user sees updated.","choices":[{"id":"a","label":"Japanese only","effect":"The English screen keeps the old label."},{"id":"b","label":"Both languages","effect":"Both screens show the new label."}]}],"request_kind":"change","needs_design":true,"design_reason":"approach_not_in_ticket","approach_in_ticket":false,"decision_sha256":"02add8333ac4cc3074d13dce57a131a3fe1d2322c7f762f88030d95b474df588"}`
)

func TestADecisionSealedBeforeTheJudgeStillReadsAndReEncodes(t *testing.T) {
	config, request, source := validArtifactFixture(t)
	for _, testCase := range []struct{ name, sealed string }{
		{"nothing was left to ask", goldenReadyBeforeTheJudge},
		{"the requester was asked", goldenClarificationBeforeTheJudge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var decision ReadinessDecision
			if err := json.Unmarshal([]byte(testCase.sealed), &decision); err != nil {
				t.Fatalf("a decision sealed before the judge does not parse: %v", err)
			}
			if decision.ReceptionJudgment != nil || len(decision.Assumptions) != 0 {
				t.Errorf("an old decision came back carrying fields nobody wrote into it")
			}
			for _, question := range decision.Questions {
				if question.ProposedDefault != "" {
					t.Errorf("an old question came back proposing %q", question.ProposedDefault)
				}
			}
			if err := decision.ValidateBinding(source, request, config); err != nil {
				t.Errorf("an old decision is refused at the gate: %v", err)
			}
			digest, err := readinessDecisionDigest(decision)
			if err != nil {
				t.Fatal(err)
			}
			if digest != decision.DecisionSHA256 {
				t.Errorf("the digest of an old decision moved to %s", digest)
			}
			again, err := json.Marshal(decision)
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != testCase.sealed {
				t.Errorf("re-encoding an old decision changed its bytes.\n was %s\n now %s", testCase.sealed, again)
			}
		})
	}
	// And the reading that makes the goldens worth keeping: the three tags
	// are named here, so a reader meeting a failure above knows what to
	// look at. A decision this engine seals with none of the three settled
	// carries none of the three keys.
	for _, key := range []string{"reception_judgment", "assumptions", "proposed_default"} {
		if strings.Contains(goldenClarificationBeforeTheJudge, key) {
			t.Errorf("the golden already carries %q and cannot pin its absence", key)
		}
	}
}
