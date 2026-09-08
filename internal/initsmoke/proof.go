package initsmoke

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"automation.internal/ticket-ingress/internal/githubapi"
	"automation.internal/ticket-ingress/internal/worker"
	"automation.internal/ticket-ingress/internal/worker/investigate"
)

// These are the existing controller artifact fields (cmd/controller/artifacts.go),
// kept in their digest order. No new artifact or publication authority is issued.
type deliveryBinding struct {
	DeliveryID       string   `json:"delivery_id"`
	InputSHA256      string   `json:"input_sha256"`
	ConfigSHA256     string   `json:"config_sha256"`
	ToolSHA          string   `json:"tool_sha"`
	IssueKey         string   `json:"issue_key"`
	Repository       string   `json:"repository"`
	SourceSHA256     string   `json:"source_sha256"`
	CandidateSHA256  string   `json:"candidate_sha256"`
	DecisionSHA256   string   `json:"decision_sha256"`
	ValidationSHA256 string   `json:"validation_sha256"`
	ProductPaths     []string `json:"product_paths"`
}
type featureProof struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          string          `json:"kind"`
	Binding       deliveryBinding `json:"binding"`
	Payload       struct {
		Feature     githubapi.PublishedFeature `json:"feature"`
		PullRequest githubapi.PullRequest      `json:"pull_request"`
	} `json:"payload"`
	ArtifactSHA256 string `json:"artifact_sha256"`
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || len(raw) > 8*1024*1024 {
		return errors.New("記録が無いか大きすぎます")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return errors.New("記録の形式が不正です")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("記録の後に余分なデータがあります")
	}
	return nil
}

// validateProof recomputes the normal worker gates on the artifacts actually
// read from this delivery. A PR URL or a board's done label alone is insufficient.
func validateProof(files map[string]json.RawMessage, config worker.Config, record Record) (featureProof, error) {
	var proof featureProof
	if err := decode(files["feature-pr.json"], &proof); err != nil {
		return proof, err
	}
	sealed := proof.ArtifactSHA256
	proof.ArtifactSHA256 = ""
	raw, err := json.Marshal(proof)
	proof.ArtifactSHA256 = sealed
	if err != nil || sealed != digest(string(raw)) || proof.SchemaVersion != 1 || proof.Kind != "m1-feature-pull-request" {
		return proof, errors.New("PR の確定記録が不正です")
	}
	b := proof.Binding
	configSHA, err := config.SHA256()
	if err != nil || b.ConfigSHA256 != configSHA || b.DeliveryID != record.DeliveryID || b.IssueKey != record.IssueKey || b.Repository != record.Repository || len(b.ProductPaths) != 1 || b.ProductPaths[0] != record.Path {
		return proof, errors.New("PR と動作確認の身元・編集範囲が一致しません")
	}
	var candidate worker.Candidate
	stageDir := ""
	for name, raw := range files {
		if strings.HasPrefix(name, "history/stage-") && strings.HasSuffix(name, "/candidate.json") {
			var value worker.Candidate
			if err := decode(raw, &value); err != nil {
				return proof, err
			}
			if value.CandidateSHA256 == b.CandidateSHA256 {
				if stageDir != "" {
					return proof, errors.New("候補の記録が一意ではありません")
				}
				candidate = value
				stageDir = strings.TrimSuffix(name, "candidate.json")
			}
		}
	}
	if stageDir == "" {
		return proof, errors.New("納品された候補の記録がありません")
	}
	var ticket worker.TicketRequest
	var source worker.SourceSnapshot
	var decision worker.StageDecision
	var validation worker.ValidationEvidence
	var applier worker.AgentRun
	for name, destination := range map[string]any{stageDir + "ticket.json": &ticket, stageDir + "source.json": &source, stageDir + "decision.json": &decision, "validation.json": &validation, stageDir + "applier-run.json": &applier} {
		if err := decode(files[name], destination); err != nil {
			return proof, fmt.Errorf("%s: %w", name, err)
		}
	}
	var reviews []worker.Review
	for _, endpoint := range config.Models.Reviewers {
		var review worker.Review
		if err := decode(files[stageDir+endpoint.ID+".json"], &review); err != nil {
			return proof, err
		}
		reviews = append(reviews, review)
	}
	if err := worker.ValidatePublishGate(decision, validation, candidate, reviews, source, ticket, config); err != nil {
		return proof, errors.New("候補レビューと検証の確定記録が不合格です")
	}
	if ticket.DeliveryID != record.DeliveryID || ticket.IssueKey != record.IssueKey || ticket.Repository != record.Repository || source.RepositoryID != record.RepositoryID || source.BaseBranch != record.Branch || len(source.Files) != 1 || source.Files[0].Path != record.Path || source.Files[0].Created || digest(source.Files[0].Content) != record.BeforeSHA256 || len(candidate.Files) != 1 || candidate.Files[0].Path != record.Path || candidate.Files[0].Content != record.After {
		return proof, errors.New("依頼と実際の候補差分が一致しません")
	}
	if b.InputSHA256 != ticket.InputSHA256 || b.ToolSHA != ticket.ToolSHA || b.SourceSHA256 != source.SourceSHA256 || b.DecisionSHA256 != decision.DecisionSHA256 || b.ValidationSHA256 != validation.ValidationSHA256 {
		return proof, errors.New("PR と候補・検証の結び付けが違います")
	}
	if config.Agents.Applier == nil || applier.Validate(config) != nil || applier.Kind != "" || applier.AgentID != config.Agents.Applier.ID || applier.ExitCode != 0 || applier.Stage != candidate.Stage || applier.DeliveryID != ticket.DeliveryID || applier.InputSHA256 != ticket.InputSHA256 || applier.ConfigSHA256 != ticket.ConfigSHA256 || applier.ToolSHA != ticket.ToolSHA || applier.BaseSHA != source.BaseSHA || len(applier.ChangedFiles) != 1 || applier.ChangedFiles[0] != record.Path {
		return proof, errors.New("当該 delivery の写し役確定記録がありません")
	}
	var design investigate.Design
	designDir := ""
	for name, raw := range files {
		if strings.HasPrefix(name, "history/design-") && strings.HasSuffix(name, "/design.json") {
			var value investigate.Design
			if err := decode(raw, &value); err != nil {
				return proof, err
			}
			if value.DesignSHA256 == candidate.DesignSHA256 {
				if designDir != "" {
					return proof, errors.New("設計の記録が一意ではありません")
				}
				design = value
				designDir = strings.TrimSuffix(name, "design.json")
			}
		}
	}
	if designDir == "" {
		return proof, errors.New("既定の設計工程を通った記録がありません")
	}
	var designDecision investigate.DesignDecision
	if err := decode(files[designDir+"decision.json"], &designDecision); err != nil {
		return proof, err
	}
	var designReviews []investigate.DesignReview
	for _, endpoint := range config.Models.Reviewers {
		var review investigate.DesignReview
		if err := decode(files[designDir+endpoint.ID+"-design-review.json"], &review); err != nil {
			return proof, err
		}
		judge, ok := config.Models.DesignReviewerFor(endpoint.ID)
		if !ok || review.ReviewerID != endpoint.ID || review.Vendor != judge.Vendor || review.Model != judge.Model || review.BaseURL != judge.BaseURL {
			return proof, errors.New("設計レビュー役の身元が違います")
		}
		designReviews = append(designReviews, review)
	}
	identity := investigate.Identity{DeliveryID: ticket.DeliveryID, InputSHA256: ticket.InputSHA256, ConfigSHA256: ticket.ConfigSHA256, ToolSHA: ticket.ToolSHA, BaseSHA: source.BaseSHA}
	var investigation investigate.Investigation
	if err := decode(files[designDir+"investigation.json"], &investigation); err != nil {
		return proof, err
	}
	if investigation.ValidateBinding(identity) != nil || design.ValidateBinding(identity, investigation) != nil {
		return proof, errors.New("調査と設計の確定記録が一致しません")
	}
	if designDecision.Validate(identity, investigate.DesignSubject(design), designReviews, config.DesignRounds()) != nil || worker.ValidateDesignBinding(candidate, design, worker.DesignDecisionSummary{Subject: designDecision.Subject, SubjectSHA256: designDecision.SubjectSHA256, Outcome: designDecision.Outcome}) != nil {
		return proof, errors.New("設計レビューの合格と納品候補が一致しません")
	}
	checkedBase := validation.CheckedOutSHA
	if checkedBase == "" {
		checkedBase = validation.BaseSHA
	}
	feature, pr := proof.Payload.Feature, proof.Payload.PullRequest
	if feature.Base.SHA != checkedBase || feature.HeadSHA != pr.HeadSHA || feature.Branch != pr.HeadRef || pr.BaseRef != record.Branch || pr.BaseSHA != checkedBase || !strings.EqualFold(pr.HeadFullName, record.Repository) || len(feature.Paths) != 1 || feature.Paths[0] != record.Path || pr.Number <= 0 || !strings.EqualFold(pr.HTMLURL, fmt.Sprintf("https://github.com/%s/pull/%d", record.Repository, pr.Number)) {
		return proof, errors.New("PR の枝・head・検証した base が一致しません")
	}
	return proof, nil
}
