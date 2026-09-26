package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// History is evidence, not authority. It explains what earlier attempts did
// without making their objections or rulings apply to the current candidate.
// The current round's fully bound artifacts still determine what may be ruled
// on. Keep the projection, not every earlier file's contents, in the prompt
// and the ruling so a repeated attempt cannot grow memory without bound.
const maxArbitrationHistoryBytes = 64 * 1024

type ArbitrationHistory struct {
	DeliveryID   string             `json:"delivery_id"`
	InputSHA256  string             `json:"input_sha256"`
	ConfigSHA256 string             `json:"config_sha256"`
	ToolSHA      string             `json:"tool_sha"`
	Rounds       []ArbitrationRound `json:"rounds"`
	Unavailable  []int              `json:"unavailable_rounds,omitempty"`
	Omitted      []int              `json:"omitted_for_size,omitempty"`
}

type ArbitrationRound struct {
	Round           int                     `json:"round"`
	CandidateSHA256 string                  `json:"candidate_sha256"`
	GeneratedAt     time.Time               `json:"generated_at"`
	BaseSHA         string                  `json:"base_sha"`
	Files           []string                `json:"files"`
	Rationale       string                  `json:"implementer_rationale"`
	Reviews         []Review                `json:"reviews"`
	Decision        string                  `json:"decision"`
	Ruling          *ArbitrationPriorRuling `json:"ruling,omitempty"`
	Validation      *ValidationFailure      `json:"refused_validation,omitempty"`
	Unavailable     []string                `json:"unavailable_evidence,omitempty"`
}

// Do not nest a ruling's own history: that would duplicate every earlier
// context into the next one. Its digest points to the complete sealed record.
type ArbitrationPriorRuling struct {
	SHA256      string              `json:"ruling_sha256"`
	Ruling      string              `json:"ruling"`
	Instruction string              `json:"instruction,omitempty"`
	Overruled   []OverruledFinding  `json:"overruled,omitempty"`
	Assumption  ReadinessAssumption `json:"assumption"`
}

// LoadArbitrationHistory reuses the trail's artifact verification and binds
// each earlier request to this delivery. A bad older record is explicitly
// unavailable; it does not prevent ruling on a sound current round. Recent
// whole rounds take priority when the context does not all fit. No record is
// repaired, overwritten or deleted while building this read-only projection.
func LoadArbitrationHistory(dir string, before int, request TicketRequest, config Config) (*ArbitrationHistory, error) {
	if before < 1 || before > StageCeiling {
		return nil, errors.New("arbitration history round is invalid")
	}
	if dir == "" || before == 1 {
		return nil, nil
	}
	history := &ArbitrationHistory{DeliveryID: request.DeliveryID, InputSHA256: request.InputSHA256,
		ConfigSHA256: request.ConfigSHA256, ToolSHA: request.ToolSHA, Rounds: []ArbitrationRound{}}
	for number := before - 1; number >= 1; number-- {
		stageDir := filepath.Join(dir, "stage-"+strconv.Itoa(number))
		stage, err := loadTrailStage(stageDir, number, config, request.ToolSHA)
		if err != nil || stage.Request.DeliveryID != request.DeliveryID || stage.Request.InputSHA256 != request.InputSHA256 ||
			stage.Request.ConfigSHA256 != request.ConfigSHA256 || stage.Request.Repository != request.Repository || stage.Request.Request != request.Request {
			history.Unavailable = append(history.Unavailable, number)
			continue
		}
		entry := ArbitrationRound{Round: number, CandidateSHA256: stage.Candidate.CandidateSHA256,
			GeneratedAt: stage.Candidate.GeneratedAt, BaseSHA: stage.Source.BaseSHA,
			Rationale: stage.Candidate.Rationale, Reviews: stage.Reviews, Decision: stage.Decision.Outcome}
		for _, file := range stage.Candidate.Files {
			entry.Files = append(entry.Files, file.Path)
		}
		ruling, err := ReadRulingFile(filepath.Join(stageDir, RulingFileName))
		if err != nil || (ruling != nil && ruling.Validate(stage.Candidate, stage.Reviews, stage.Request, config) != nil) {
			entry.Unavailable = append(entry.Unavailable, "ruling")
		} else if ruling != nil {
			entry.Ruling = &ArbitrationPriorRuling{SHA256: ruling.RulingSHA256, Ruling: ruling.Ruling,
				Instruction: ruling.Instruction, Overruled: ruling.Overruled, Assumption: ruling.Assumption}
		}
		failurePath := filepath.Join(stageDir, "validation-failure.json")
		if _, err := os.Lstat(failurePath); !errors.Is(err, os.ErrNotExist) {
			failure, readErr := ReadValidationFailureFile(failurePath)
			if err != nil || readErr != nil || !failure.Bound(number) || failure.DeliveryID != request.DeliveryID ||
				failure.InputSHA256 != request.InputSHA256 || failure.ConfigSHA256 != request.ConfigSHA256 || failure.ToolSHA != request.ToolSHA {
				entry.Unavailable = append(entry.Unavailable, "validation")
			} else {
				entry.Validation = &failure
			}
		}
		history.Rounds = append(history.Rounds, entry)
		encoded, err := json.Marshal(history)
		// Reserve space for every omitted/unavailable round number, even
		// when no further round's content fits.
		if err != nil || len(encoded) > maxArbitrationHistoryBytes-StageCeiling*16 {
			history.Rounds = history.Rounds[:len(history.Rounds)-1]
			history.Omitted = append(history.Omitted, number)
		}
	}
	sort.Slice(history.Rounds, func(i, j int) bool { return history.Rounds[i].Round < history.Rounds[j].Round })
	sort.Ints(history.Unavailable)
	sort.Ints(history.Omitted)
	if err := history.validate(before, request, config); err != nil {
		return nil, err
	}
	return history, nil
}

func (h *ArbitrationHistory) validate(before int, request TicketRequest, config Config) error {
	if h == nil {
		return nil // A ruling made before history was carried keeps its bytes.
	}
	invalid := errors.New("arbitration history is invalid")
	if before < 1 || before > StageCeiling || h.DeliveryID != request.DeliveryID || h.InputSHA256 != request.InputSHA256 ||
		h.ConfigSHA256 != request.ConfigSHA256 || h.ToolSHA != request.ToolSHA {
		return invalid
	}
	encoded, err := json.Marshal(h)
	if err != nil || len(encoded) > maxArbitrationHistoryBytes {
		return invalid
	}
	seen := make(map[int]bool, before-1)
	add := func(number int) bool {
		if number < 1 || number >= before || seen[number] {
			return false
		}
		seen[number] = true
		return true
	}
	last := 0
	for _, round := range h.Rounds {
		if !add(round.Round) || round.Round <= last || !sha256Pattern.MatchString(round.CandidateSHA256) ||
			len(round.Files) == 0 || round.GeneratedAt.IsZero() || validatePlainText(round.Rationale, 4096, true) != nil ||
			len(round.Reviews) != len(config.Models.Reviewers) || (round.Decision != "revise" && round.Decision != "converged") {
			return invalid
		}
		last = round.Round
		prior := request
		prior.TargetFiles = round.Files
		candidate := Candidate{Stage: round.Round, CandidateSHA256: round.CandidateSHA256, GeneratedAt: round.GeneratedAt}
		for index, seat := range config.Models.Reviewers {
			if round.Reviews[index].Validate(seat, candidate, prior) != nil {
				return invalid
			}
		}
		if ruling := round.Ruling; ruling != nil {
			if !sha256Pattern.MatchString(ruling.SHA256) || validateRulingBody(ruling.Ruling, ruling.Instruction, ruling.Overruled, ruling.Assumption) != nil {
				return invalid
			}
		}
		if failure := round.Validation; failure != nil && (!failure.Bound(round.Round) || failure.DeliveryID != request.DeliveryID ||
			failure.InputSHA256 != request.InputSHA256 || failure.ConfigSHA256 != request.ConfigSHA256 || failure.ToolSHA != request.ToolSHA) {
			return invalid
		}
		for _, unavailable := range round.Unavailable {
			if unavailable != "ruling" && unavailable != "validation" {
				return invalid
			}
		}
	}
	for _, numbers := range [][]int{h.Unavailable, h.Omitted} {
		if !sort.IntsAreSorted(numbers) {
			return invalid
		}
		for _, number := range numbers {
			if !add(number) {
				return invalid
			}
		}
	}
	if len(seen) != before-1 {
		return invalid // A missing round must be identified, not silently lost.
	}
	return nil
}
