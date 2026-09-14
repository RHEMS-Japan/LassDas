package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"automation.internal/ticket-ingress/internal/worker"
)

// deployScope is what a destination's deployment reacts to, as the consumer
// config declares it under `github_contract.<workflow>.deploy_paths`. Known
// is false when the destination declared nothing — an unknown scope is not
// an empty one, so the runner then waits for a run the way it always has.
type deployScope struct {
	Patterns []string
	Known    bool
}

// consumerDeployScope reads the declared scope for one phase. Staging is the
// one staging workflow. Production is every production workflow, and its
// scope is known only when each of them declares one: a guard workflow
// without a declaration could still react to the change.
func consumerDeployScope(consumerConfigPath, repository, phase string) (deployScope, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return deployScope{}, errors.New("consumer config unreadable")
	}
	type workflow struct {
		DeployPaths []string `json:"deploy_paths"`
	}
	var parsed struct {
		Consumers []struct {
			Repository string `json:"repository"`
			GitHub     struct {
				StagingWorkflow     workflow   `json:"staging_workflow"`
				ProductionWorkflows []workflow `json:"production_workflows"`
			} `json:"github_contract"`
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return deployScope{}, errors.New("consumer config invalid")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository != repository {
			continue
		}
		switch phase {
		case "staging":
			patterns := consumer.GitHub.StagingWorkflow.DeployPaths
			return deployScope{Patterns: patterns, Known: len(patterns) > 0}, nil
		case "production":
			if len(consumer.GitHub.ProductionWorkflows) == 0 {
				return deployScope{}, nil
			}
			scope := deployScope{Known: true}
			for _, w := range consumer.GitHub.ProductionWorkflows {
				if len(w.DeployPaths) == 0 {
					return deployScope{}, nil
				}
				scope.Patterns = append(scope.Patterns, w.DeployPaths...)
			}
			return scope, nil
		default:
			return deployScope{}, errors.New("deploy phase is invalid")
		}
	}
	return deployScope{}, errors.New("consumer is not configured")
}

// deployPathCovered reports whether one delivered path falls inside the
// declared scope, with the reading the config package gives deploy_paths
// (prefixes, "*" within a segment, "**" across segments, trailing "/").
func deployPathCovered(patterns []string, filePath string) bool {
	return worker.DeployPathCovered(patterns, filePath)
}

// deployNotApplicable answers, before the runner waits for a deployment,
// whether the destination's declared scope says none will come: the scope
// is known and not one delivered path falls inside it. Anything unknown —
// no declaration, no readable path list — answers false, and the runner
// waits as before, including when the consumer config cannot be read: the
// controller validates that same config and reports it properly.
func (p *Pipeline) deployNotApplicable(phase string) (bool, string, error) {
	repository, err := p.readJSONField("feature-pr.json", "binding", "repository")
	if err != nil || repository == "" {
		return false, "", nil
	}
	raw, err := readWorkspaceFile(p.path("feature-pr.json"), maxWorkspaceReadBytes)
	if err != nil {
		return false, "", nil
	}
	var wrapper struct {
		Binding struct {
			ProductPaths []string `json:"product_paths"`
		} `json:"binding"`
	}
	if json.Unmarshal(raw, &wrapper) != nil || len(wrapper.Binding.ProductPaths) == 0 {
		return false, "", nil
	}
	scope, err := consumerDeployScope(p.Config.ConsumerConfigPath, repository, phase)
	if err != nil {
		if err.Error() == "deploy phase is invalid" {
			return false, "", err
		}
		// A config this reader cannot use is not a declaration: wait, and
		// let the controller — which validates the same config — say what
		// is wrong with it.
		return false, "", nil
	}
	if !scope.Known {
		return false, "", nil
	}
	// What the phase actually changes. The staging merge is the feature
	// PR: its product paths. The promotion carries those AND the CI digest
	// files the staging deployment committed (see promotionHold) — a
	// manifest under deploy/ that a production workflow very much reacts
	// to, so leaving it out would end a delivery whose deployment runs.
	changed := append([]string(nil), wrapper.Binding.ProductPaths...)
	if phase == "production" {
		changed = append(changed, p.consumerDigestPaths()...)
	}
	for _, filePath := range changed {
		if deployPathCovered(scope.Patterns, filePath) {
			return false, "", nil
		}
	}
	label := "ステージング"
	if phase == "production" {
		label = "本番"
	}
	detail := fmt.Sprintf("変更したファイル %d 件はすべて、設定された%s配布の対象範囲（%s）の外です。配布の実行を待たずに完了します。",
		len(changed), label, strings.Join(scope.Patterns, ", "))
	return true, detail, nil
}
