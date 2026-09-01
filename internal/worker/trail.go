package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The trail is the requester-facing record of what the automation actually
// did: which rounds ran, what each reviewer objected to, what changed, which
// adopted decisions bound the work and whether the independent validation
// passed. It exists because a terminal comment that says only "success" plus
// a run link leaves no durable evidence on the ticket — the run's logs expire
// and require GitHub access, and the requester's surface is the ticket
// (observed on the first delivered PR).
//
// Every rendered fragment comes from sealed, schema-validated artifacts:
// outcomes and verdicts are enums, finding codes match the identifier
// pattern, paths are target-validated, and the free-text fields (finding
// messages, the implementer's rationale) passed the same plain-text bounds
// the pipeline enforces everywhere else.

const (
	// MaxTrailBytes bounds the rendered trail. Terminal comments and pull
	// request bodies both carry it verbatim.
	MaxTrailBytes = 6 * 1024

	trailFindingRunes   = 240
	trailRationaleRunes = 700
)

// trailStage is one loaded, cross-validated stage of the model history.
type trailStage struct {
	Stage     int
	Candidate Candidate
	Reviews   []Review
	Decision  StageDecision
	Source    SourceSnapshot
	Request   TicketRequest
}

// TrailSummary is the bounded read projection used by the local operations
// console. It is derived only after LoadTrailStages has revalidated the sealed
// ticket/source/candidate/review/decision chain.
type TrailSummary struct {
	Cycles   []TrailCycle
	Decision string
}

type TrailCycle struct {
	Number         int
	Reviews        []TrailReview
	Findings       int
	DurationMillis int64
}

type TrailReview struct {
	Reviewer, Verdict string
	Findings          int
	DurationMillis    int64
}

func LoadTrailSummary(historyDir string, config Config, toolSHA string) (TrailSummary, error) {
	stages, err := LoadTrailStages(historyDir, config, toolSHA)
	if err != nil {
		return TrailSummary{}, err
	}
	return summarizeTrailStages(stages), nil
}

func summarizeTrailStages(stages []trailStage) TrailSummary {
	summary := TrailSummary{Decision: stages[len(stages)-1].Decision.Outcome}
	for _, stage := range stages {
		cycle := TrailCycle{Number: stage.Stage}
		for _, review := range stage.Reviews {
			cycle.Reviews = append(cycle.Reviews, TrailReview{Reviewer: review.ReviewerID, Verdict: review.Verdict, Findings: len(review.Findings), DurationMillis: review.Invocation.LatencyMillis})
			cycle.Findings += len(review.Findings)
			cycle.DurationMillis += review.Invocation.LatencyMillis
		}
		summary.Cycles = append(summary.Cycles, cycle)
	}
	return summary
}

// LoadTrailStages reads stage-1..N from a model history directory, validating
// every artifact against the sealed chain before it may appear in a trail. A
// missing stage-1 is an error; the trail of a run that never implemented
// anything is not this function's business.
func LoadTrailStages(historyDir string, config Config, toolSHA string) ([]trailStage, error) {
	stages := make([]trailStage, 0, config.MaxStages)
	for number := 1; number <= config.MaxStages; number++ {
		stageDir := filepath.Join(historyDir, "stage-"+strconv.Itoa(number))
		if _, err := os.Stat(filepath.Join(stageDir, "decision.json")); err != nil {
			break
		}
		var request TicketRequest
		if err := ReadJSONFile(filepath.Join(stageDir, "ticket.json"), MaxTicketJSONBytes, &request); err != nil {
			return nil, errors.New("trail stage ticket could not be read")
		}
		if err := request.Validate(config); err != nil || request.ToolSHA != toolSHA {
			return nil, errors.New("trail stage ticket was rejected")
		}
		var source SourceSnapshot
		if err := ReadJSONFile(filepath.Join(stageDir, "source.json"), MaxArtifactJSONBytes, &source); err != nil {
			return nil, errors.New("trail stage source could not be read")
		}
		if err := source.Validate(request, config); err != nil {
			return nil, errors.New("trail stage source was rejected")
		}
		var candidate Candidate
		if err := ReadJSONFile(filepath.Join(stageDir, "candidate.json"), MaxArtifactJSONBytes, &candidate); err != nil {
			return nil, errors.New("trail stage candidate could not be read")
		}
		reviews := make([]Review, 0, len(config.Models.Reviewers))
		for _, endpoint := range config.Models.Reviewers {
			var review Review
			if err := ReadJSONFile(filepath.Join(stageDir, endpoint.ID+".json"), MaxReviewJSONBytes, &review); err != nil {
				return nil, errors.New("trail stage review could not be read")
			}
			reviews = append(reviews, review)
		}
		decision, err := DecideStage(candidate, reviews, source, request, config)
		if err != nil {
			return nil, errors.New("trail stage did not rederive")
		}
		var sealed StageDecision
		if err := ReadJSONFile(filepath.Join(stageDir, "decision.json"), MaxReviewJSONBytes, &sealed); err != nil {
			return nil, errors.New("trail stage decision could not be read")
		}
		if sealed.DecisionSHA256 != decision.DecisionSHA256 {
			return nil, errors.New("trail stage decision does not match")
		}
		stages = append(stages, trailStage{
			Stage: number, Candidate: candidate, Reviews: reviews, Decision: decision,
			Source: source, Request: request,
		})
	}
	if len(stages) == 0 {
		return nil, errors.New("trail has no stages")
	}
	return stages, nil
}

// ComposeTrail renders the loaded stages, the adopted clarification decisions
// and the validation outcome as the requester-facing record. The result is
// deterministic and bounded by MaxTrailBytes.
func ComposeTrail(stages []trailStage, clarification *ClarificationContext, validationPassed bool) string {
	var builder strings.Builder
	final := stages[len(stages)-1]

	fmt.Fprintf(&builder, "### 実装とレビューの経過 (%d 周で%s)\n", len(stages), trailOutcomeLabel(final.Decision.Outcome))
	for _, stage := range stages {
		fmt.Fprintf(&builder, "- %d 周目: ", stage.Stage)
		parts := make([]string, 0, len(stage.Reviews))
		for _, review := range stage.Reviews {
			if review.Verdict == "pass" {
				parts = append(parts, review.ReviewerID+" = 指摘なし")
			} else {
				parts = append(parts, fmt.Sprintf("%s = 指摘 %d 件", review.ReviewerID, len(review.Findings)))
			}
		}
		builder.WriteString(strings.Join(parts, " / "))
		builder.WriteString(" → " + trailOutcomeLabel(stage.Decision.Outcome) + "\n")
		for _, review := range stage.Reviews {
			for _, finding := range review.Findings {
				fmt.Fprintf(&builder, "  - %s: %s\n", finding.Code, trailClip(finding.Message, trailFindingRunes))
			}
		}
	}

	paths := make([]string, 0, len(final.Candidate.Files))
	changedLines := 0
	for index, file := range final.Candidate.Files {
		if index < len(final.Source.Files) && file.Content != final.Source.Files[index].Content {
			lines, _, _ := conservativeChangeBudget(final.Source.Files[index].Content, file.Content)
			changedLines += lines
			paths = append(paths, file.Path)
		}
	}
	sort.Strings(paths)
	fmt.Fprintf(&builder, "\n### 変更内容 (%d ファイル・約 %d 行)\n%s\n", len(paths), changedLines, strings.Join(paths, "\n"))
	if final.Candidate.Rationale != "" {
		builder.WriteString("\n実装者の説明 (要点): " + trailClip(final.Candidate.Rationale, trailRationaleRunes) + "\n")
	}

	if clarification != nil && len(clarification.Exchanges) > 0 {
		builder.WriteString("\n### 反映した決定事項 (質問への回答)\n")
		for _, exchange := range clarification.Exchanges {
			for _, question := range exchange.Questions {
				answer := exchange.Answers[question.ID]
				label := answer
				for _, choice := range question.Choices {
					if choice.ID == answer {
						label = answer + ": " + choice.Label
					}
				}
				fmt.Fprintf(&builder, "- %s (%s) → %s\n", trailClip(question.Question, 160), question.ID, trailClip(label, 200))
			}
		}
	}

	builder.WriteString("\n### 独立検証 (隔離サンドボックス)\n")
	if validationPassed {
		builder.WriteString("- ビルドとテストを通過\n")
	} else {
		builder.WriteString("- 未実施または未通過 (この記録の時点では PR は検証を通っていません)\n")
	}

	return trailTruncate(builder.String())
}

func trailOutcomeLabel(outcome string) string {
	switch outcome {
	case "converged":
		return "収束"
	case "revise":
		return "やり直し"
	case "nonconverged":
		return "収束せず"
	default:
		return outcome
	}
}

func trailClip(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func trailTruncate(value string) string {
	if len(value) <= MaxTrailBytes {
		return value
	}
	clipped := value[:MaxTrailBytes-len("\n…(切り詰め)")]
	for len(clipped) > 0 && !strings.HasSuffix(clipped, "\n") {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + "\n…(切り詰め)"
}
