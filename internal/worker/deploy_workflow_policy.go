package worker

import (
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The one file the engine cannot write, and the permission that lets it.
//
// A destination that asks for production needs something that deploys it,
// and on this platform that something is a file under a dotted directory.
// The whole path vocabulary refuses a dotted directory, deliberately and in
// eleven places: a file there runs on the destination's own runners, with
// the destination's own secrets, the moment a branch is pushed. An engine
// that could write one could write itself a way to read every secret the
// repository holds.
//
// So the floor stays, and what opens it is not a flag. An operator who
// wants the engine to author the deploy workflow says what such a file may
// contain: which events may start it, how much the job's token may do,
// which secrets it may name, which third-party actions it may run and at
// which exact commit, which runners it may ask for, and which file names it
// may take. Every one of those is a sentence about content, so the
// permission cannot be given by accident and cannot be given open-ended.
//
// The policy is checked twice. Here, when the configuration loads, so a
// policy that contradicts itself never reaches a run; and again over the
// parsed workflow before the candidate is sealed, so what was written is
// held to what was allowed.

// DeployWorkflowsKind is what the means is called where resources are
// named. It is the same word the configuration key uses, so a report that
// says a part was not applied names something an operator can search for.
const DeployWorkflowsKind = "deploy_workflows"

// WorkflowDirectory is where this platform keeps the files that deploy. It
// is the only dotted directory anything in the engine may address, and only
// for the file names one sealed plan carries.
const WorkflowDirectory = ".github/workflows/"

// MaxDeployWorkflowPaths bounds how many workflow files one destination may
// let the engine author. A release path is a handful of files; a policy
// naming more than this is not describing one.
const MaxDeployWorkflowPaths = 8

// MaxDeployWorkflowBytes bounds one workflow file the gate will parse. A
// deploy workflow is short, and a parser is the one place where size is an
// attack rather than an inconvenience.
const MaxDeployWorkflowBytes = 64 * 1024

// The events a workflow may be started by. push and pull_request are
// admitted only with a branch filter naming the destination's own release
// or integration branch, which is checked against the destination rather
// than against the policy: the branches are already written down once, and
// a second copy would be a second thing to keep true.
const (
	WorkflowTriggerPush        = "push"
	WorkflowTriggerPullRequest = "pull_request"
	WorkflowTriggerDispatch    = "workflow_dispatch"
)

// deployWorkflowTriggers are the events a policy may name at all. Anything
// outside it is refused when the configuration loads rather than when a
// workflow is written, because an event nobody thought about is an event
// nobody bounded: repository_dispatch, issue_comment and pull_request_target
// all start a job from something a stranger can send.
// Three, and only these three. Each of the others starts a job from
// something outside the branch filter this gate checks — a release being
// published, another workflow finishing, a deployment somebody created —
// so admitting one would be admitting a start nothing here bounds.
var deployWorkflowTriggers = map[string]bool{
	WorkflowTriggerPush: true, WorkflowTriggerPullRequest: true, WorkflowTriggerDispatch: true,
}

// workflowPermissionScopes are the permissions a job's token can be given.
// Spelled out so a scope this engine has never heard of is refused rather
// than passed through: an unknown scope in a policy is a permission nobody
// bounded, and one in a workflow is a permission nobody allowed.
var workflowPermissionScopes = map[string]bool{
	"actions": true, "attestations": true, "checks": true, "contents": true,
	"deployments": true, "discussions": true, "id-token": true, "issues": true,
	"models": true, "packages": true, "pages": true, "pull-requests": true,
	"repository-projects": true, "security-events": true, "statuses": true,
}

// workflowPermissionLevels are the three values a scope can hold, in order
// of how much they grant. A workflow asking for more than the policy's
// level for a scope is refused; asking for less is ordinary.
var workflowPermissionLevels = map[string]int{"none": 0, "read": 1, "write": 2}

var (
	// A hosted runner's label is the platform's own: the three families
	// GitHub runs itself. Anything else — self-hosted, a bare word, a group
	// name — names a machine the destination owns, and a run: step an AI
	// wrote would then be executing inside the destination's own network.
	hostedRunnerPattern = regexp.MustCompile(`^(?:ubuntu|windows|macos)-[a-z0-9][a-z0-9.-]{0,30}$`)
	// An action is owner/repo, optionally with a path inside it, pinned to
	// a full commit id. A tag or a branch is a moving target: what the
	// policy allowed today is not what runs tomorrow.
	pinnedActionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,38}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}(?:/[A-Za-z0-9][A-Za-z0-9._/-]{0,199})?@[0-9a-f]{40}$`)
	// A secret's name as this platform writes it.
	workflowSecretPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
)

// forbiddenWorkflowSecrets are the names a policy may never allow a
// workflow to read.
//
// The first is the engine's own credential for this destination: the token
// it pushes branches and opens pull requests with. A workflow that could
// read it could open a pull request as the engine, and the engine is the
// thing that merges. The rest are the engine's own two namespaces and the
// platform's built-in token, which is granted through permissions: rather
// than named as a secret, so naming it is always a mistake.
var forbiddenWorkflowSecrets = map[string]bool{
	"TARGET_GITHUB_TOKEN": true,
	"GITHUB_TOKEN":        true,
	"BACKLOG_API_KEY":     true,
}

var forbiddenWorkflowSecretPrefixes = []string{"LASSDAS_", "HERMES_", "MODEL_API_KEY"}

// ForbiddenWorkflowSecret reports whether a secret name is one no workflow
// this engine writes may reference, whatever a policy says.
func ForbiddenWorkflowSecret(name string) bool {
	upper := strings.ToUpper(name)
	if forbiddenWorkflowSecrets[upper] {
		return true
	}
	for _, prefix := range forbiddenWorkflowSecretPrefixes {
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// DeployWorkflowPolicy is what a destination lets an engine-authored deploy
// workflow contain. It is a policy and never a boolean: every field bounds
// something a workflow can do, and a field left empty bounds it to nothing.
type DeployWorkflowPolicy struct {
	// Paths are the file names, under the workflow directory, the engine
	// may create. Names only — the directory is fixed, because it is the
	// only dotted directory anything here may address.
	Paths []string `json:"paths"`
	// Triggers are the events an authored workflow may declare. push and
	// pull_request additionally need a branch filter naming only the
	// destination's release or integration branch, which is checked where
	// the workflow is read rather than written down again here.
	Triggers []string `json:"triggers"`
	// Permissions is the most any job's token may be given, scope by scope.
	// A scope absent from it may not be requested at all. An empty map is a
	// policy, not an omission: it says the token gets nothing.
	Permissions map[string]string `json:"permissions"`
	// Secrets are the secret names a workflow may reference. Empty means it
	// may reference none, which is the ordinary case for a workflow that
	// deploys through an identity token.
	Secrets []string `json:"secrets,omitempty"`
	// Actions are the third-party actions a workflow may run, each pinned
	// to a commit id. This list does more work than the secrets list: an
	// action sees the whole job — its token, its environment, its
	// checkout — so allowing one is allowing its author.
	Actions []string `json:"actions,omitempty"`
	// Runners are the labels a job may ask for, and they are hosted labels.
	// A self-hosted runner executes the job inside the destination's own
	// infrastructure, which is not somewhere an AI-authored run: step
	// belongs.
	Runners []string `json:"runners"`
}

// Handed reports whether this destination handed the engine the means to
// author deploy workflows.
func (c ConsumerConfig) Handed() bool { return c.DeployWorkflows() != nil }

// DeployWorkflows is the destination's content policy for engine-authored
// deploy workflows, or nil when the means was not handed — which is every
// destination configured before this existed.
func (c ConsumerConfig) DeployWorkflows() *DeployWorkflowPolicy {
	if c.Infrastructure == nil {
		return nil
	}
	return c.Infrastructure.DeployWorkflows
}

// WorkflowPath is the repository path of one file the policy names.
func WorkflowPath(name string) string { return WorkflowDirectory + name }

// Allows reports whether the policy names this repository path.
func (p *DeployWorkflowPolicy) Allows(repositoryPath string) bool {
	if p == nil {
		return false
	}
	name, ok := strings.CutPrefix(repositoryPath, WorkflowDirectory)
	return ok && slices.Contains(p.Paths, name)
}

// AllowsSecret reports whether a workflow may reference this secret name.
func (p *DeployWorkflowPolicy) AllowsSecret(name string) bool {
	return p != nil && !ForbiddenWorkflowSecret(name) && slices.Contains(p.Secrets, name)
}

// AllowsAction reports whether a workflow may run this uses: value. The
// comparison is exact, pin included: an action allowed at one commit is not
// allowed at another.
func (p *DeployWorkflowPolicy) AllowsAction(uses string) bool {
	return p != nil && slices.Contains(p.Actions, uses)
}

// AllowsRunner reports whether a job may ask for this label.
func (p *DeployWorkflowPolicy) AllowsRunner(label string) bool {
	return p != nil && hostedRunnerPattern.MatchString(label) && slices.Contains(p.Runners, label)
}

// AllowsTrigger reports whether a workflow may declare this event.
func (p *DeployWorkflowPolicy) AllowsTrigger(event string) bool {
	return p != nil && slices.Contains(p.Triggers, event)
}

// PermissionCeiling is the most a workflow may ask for one scope. A scope
// the policy does not name has a ceiling of none.
func (p *DeployWorkflowPolicy) PermissionCeiling(scope string) int {
	if p == nil {
		return 0
	}
	return workflowPermissionLevels[p.Permissions[scope]]
}

// RepositoryPaths are the files this policy names, as repository paths, in
// the order the configuration wrote them.
func (p *DeployWorkflowPolicy) RepositoryPaths() []string {
	if p == nil {
		return nil
	}
	paths := make([]string, 0, len(p.Paths))
	for _, name := range p.Paths {
		paths = append(paths, WorkflowPath(name))
	}
	return paths
}

// ValidateDeployWorkflowPolicy is the check the setup runs before it writes
// a configuration, so a mistyped answer is named where it was written.
func ValidateDeployWorkflowPolicy(p DeployWorkflowPolicy) error { return p.validate() }

// validate refuses a policy that could not mean what it says.
//
// Three refusals are the reason this type exists rather than a boolean. A
// policy with nothing in it would be a boolean wearing a policy's clothes —
// it would hand the means and bound nothing. A policy naming the engine's
// own token would hand a workflow the credential the engine merges with. A
// policy admitting a self-hosted runner would let an AI-authored run: step
// execute inside the destination's own network. None of the three is a
// thing an operator can mean, so none of them loads.
func (p DeployWorkflowPolicy) validate() error {
	if len(p.Paths) == 0 || len(p.Paths) > MaxDeployWorkflowPaths {
		return errors.New("deploy workflow policy names no files it may create")
	}
	seenPath := make(map[string]bool, len(p.Paths))
	for _, name := range p.Paths {
		if !validWorkflowFilename(name) || path.Base(name) != name {
			return errors.New("deploy workflow policy file name is invalid")
		}
		if seenPath[name] {
			return errors.New("deploy workflow policy names a file twice")
		}
		seenPath[name] = true
	}
	if len(p.Triggers) == 0 || len(p.Triggers) > len(deployWorkflowTriggers) {
		return errors.New("deploy workflow policy allows no trigger")
	}
	seenTrigger := make(map[string]bool, len(p.Triggers))
	for _, event := range p.Triggers {
		if !deployWorkflowTriggers[event] {
			return errors.New("deploy workflow policy trigger is not one the engine bounds")
		}
		if seenTrigger[event] {
			return errors.New("deploy workflow policy names a trigger twice")
		}
		seenTrigger[event] = true
	}
	if p.Permissions == nil {
		return errors.New("deploy workflow policy states no permission ceiling")
	}
	if len(p.Permissions) > len(workflowPermissionScopes) {
		return errors.New("deploy workflow policy permission is invalid")
	}
	for scope, level := range p.Permissions {
		if !workflowPermissionScopes[scope] {
			return errors.New("deploy workflow policy permission scope is not one the engine knows")
		}
		if _, known := workflowPermissionLevels[level]; !known {
			return errors.New("deploy workflow policy permission level is invalid")
		}
	}
	if len(p.Secrets) > 32 {
		return errors.New("deploy workflow policy names too many secrets")
	}
	seenSecret := make(map[string]bool, len(p.Secrets))
	for _, name := range p.Secrets {
		if !workflowSecretPattern.MatchString(name) {
			return errors.New("deploy workflow policy secret name is invalid")
		}
		if ForbiddenWorkflowSecret(name) {
			return errors.New("deploy workflow policy names a credential the engine holds")
		}
		if seenSecret[name] {
			return errors.New("deploy workflow policy names a secret twice")
		}
		seenSecret[name] = true
	}
	if len(p.Actions) > 32 {
		return errors.New("deploy workflow policy names too many actions")
	}
	seenAction := make(map[string]bool, len(p.Actions))
	for _, uses := range p.Actions {
		if !pinnedActionPattern.MatchString(uses) {
			return errors.New("deploy workflow policy action is not pinned to a commit")
		}
		if seenAction[uses] {
			return errors.New("deploy workflow policy names an action twice")
		}
		seenAction[uses] = true
	}
	if len(p.Runners) == 0 || len(p.Runners) > 16 {
		return errors.New("deploy workflow policy allows no runner")
	}
	seenRunner := make(map[string]bool, len(p.Runners))
	for _, label := range p.Runners {
		if !hostedRunnerPattern.MatchString(label) {
			return errors.New("deploy workflow policy allows a runner the engine does not hold to be hosted")
		}
		if seenRunner[label] {
			return errors.New("deploy workflow policy names a runner twice")
		}
		seenRunner[label] = true
	}
	return nil
}
