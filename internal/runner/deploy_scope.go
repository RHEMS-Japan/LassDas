package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
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
// declared scope. A pattern is a path prefix — "docs/" or "docs" covers
// docs/README.md and docs itself, never docs2/ — or, when it carries a glob
// character, a path.Match pattern against the whole path.
func deployPathCovered(patterns []string, filePath string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if strings.ContainsAny(pattern, "*?[") {
			if ok, err := path.Match(pattern, filePath); err == nil && ok {
				return true
			}
			continue
		}
		prefix := strings.TrimSuffix(pattern, "/")
		if filePath == prefix || strings.HasPrefix(filePath, prefix+"/") {
			return true
		}
	}
	return false
}

// deployNotApplicable answers, before the runner waits for a deployment,
// whether the destination's declared scope says none will come: the scope
// is known and not one delivered path falls inside it. Anything unknown —
// no declaration, no readable path list — answers false, and the runner
// waits as before. A consumer config it cannot read is broken plumbing and
// is returned as such.
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
		return false, "", err
	}
	if !scope.Known {
		return false, "", nil
	}
	for _, filePath := range wrapper.Binding.ProductPaths {
		if deployPathCovered(scope.Patterns, filePath) {
			return false, "", nil
		}
	}
	label := "ステージング"
	if phase == "production" {
		label = "本番"
	}
	detail := fmt.Sprintf("変更したファイル %d 件はすべて、設定された%s配布の対象範囲（%s）の外です。配布の実行を待たずに完了します。",
		len(wrapper.Binding.ProductPaths), label, strings.Join(scope.Patterns, ", "))
	return true, detail, nil
}
