package worker

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// What the engine wrote is held to what it was allowed to write.
//
// The path gates decide which file may exist. They say nothing about what
// is in it, and what is in it is the whole risk: a workflow file runs on
// the destination's own account, with the destination's own secrets, from
// the moment a branch carrying it is pushed — before a person reads the
// pull request, and before anything else in this engine looks at the
// change. A file that was allowed to exist and never read is a file an
// implementing model wrote a token exfiltrator into.
//
// So every workflow file in the candidate is parsed and measured against
// the destination's policy before the candidate is sealed, which is before
// any branch carrying it is pushed. The parse is the ordinary YAML parser
// rather than a reading of the text: a hand-written subset parser that
// misreads one construct is the hole this check exists to close, and
// anything the rules below cannot classify is refused rather than skipped.
//
// The refusal is an ordinary round objection. The implementer is told which
// rule it broke and writes the file again; the ladder and the stagnation
// rules do the rest. Nothing here asks a person for anything.

// The rules, as the objection names them. They are the requester-facing
// words for what was wrong, not the field that held it.
const (
	workflowRuleShape       = "workflow の形"
	workflowRuleTrigger     = "on: の起動条件"
	workflowRulePermissions = "permissions: の権限"
	workflowRuleSecrets     = "secrets の参照"
	workflowRuleAction      = "uses: の外部 action"
	workflowRuleRunner      = "runs-on: の実行環境"
	workflowRuleRunStep     = "run: の中身"
)

// The top-level keys a workflow this engine writes may hold. Everything
// else is refused as unclassifiable: a key the rules below do not read is a
// key nothing bounds.
// defaults is absent on purpose. defaults.run.shell names the interpreter
// every run: step is handed to, so a workflow declaring one could be read
// as a shell script by every rule below and then executed as something
// else entirely — every check on what a step runs would be reading a
// different language from the one that runs.
var workflowTopLevelKeys = map[string]bool{
	"name": true, "run-name": true, "on": true, "permissions": true,
	"env": true, "concurrency": true, "jobs": true,
}

// The job keys it may hold. container and services are absent on purpose:
// both run an image of somebody's choosing alongside the job, which is the
// same risk as an unpinned action and is not something a deploy workflow
// the engine wrote needs.
var workflowJobKeys = map[string]bool{
	"name": true, "needs": true, "if": true, "permissions": true, "runs-on": true,
	"environment": true, "concurrency": true, "outputs": true, "env": true,
	"steps": true, "timeout-minutes": true, "strategy": true,
	"continue-on-error": true,
}

// The step keys it may hold.
var workflowStepKeys = map[string]bool{
	"name": true, "id": true, "if": true, "uses": true, "run": true, "with": true,
	"env": true, "shell": true, "working-directory": true, "continue-on-error": true,
	"timeout-minutes": true,
}

// workflowStepShells are the interpreters a step may name. The rules below
// read a run: step as a POSIX shell script, so a step that names anything
// else would be measured in one language and run in another.
var workflowStepShells = map[string]bool{"bash": true, "sh": true}

// environmentDumps are the shell words that print the whole environment. A
// step that runs one has put every secret the job holds into a log that
// outlives the run and that anybody who can read the repository can read.
var environmentDumps = []string{"env", "printenv", "set", "export -p", "export"}

// CheckDeployWorkflows holds every workflow file in a candidate to the
// destination's content policy.
//
// Called where the candidate is sealed, which is before the branch carrying
// it is pushed, and again by every reader of a sealed candidate. Files that
// are not workflow files are not its business and pass through untouched.
func CheckDeployWorkflows(files []CandidateFile, consumer ConsumerConfig) error {
	policy := consumer.DeployWorkflows()
	branches := releaseBranchFilter(consumer)
	for _, file := range files {
		if !strings.HasPrefix(file.Path, WorkflowDirectory) {
			continue
		}
		if policy == nil {
			// Unreachable through the gates, which refuse the path itself
			// where no policy was handed. Stated anyway: this function is
			// the last thing between a workflow file and a pushed branch,
			// and it does not rely on an earlier check having run.
			return workflowObjection(file.Path, workflowRuleShape,
				"この納品先は本体に workflow を書かせる設定になっていません。")
		}
		if err := checkOneWorkflow(file, policy, branches); err != nil {
			return err
		}
	}
	return nil
}

// releaseBranchFilter is the branches a push or pull_request trigger may be
// filtered to: this destination's own release and integration branches and
// nothing else. A workflow filtered to a feature branch would run on every
// branch this engine pushes, with the repository's secrets, before anybody
// had read what it does.
func releaseBranchFilter(consumer ConsumerConfig) []string {
	branches := make([]string, 0, 2)
	for _, branch := range []string{consumer.ReleaseBranch, consumer.IntegrationBranch} {
		if branch != "" && !contains(branches, branch) {
			branches = append(branches, branch)
		}
	}
	return branches
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// workflowObjection is the sentence the round is refused with. It names the
// file and the rule and stops there: what was actually written is the
// model's own text, and a refusal that quoted it would carry whatever it
// holds — a credential the file should not have named among it — into the
// record, the next round's instruction and the run's own log.
func workflowObjection(path, rule, detail string) error {
	return fmt.Errorf("%s が %s の決まりに反しています: %s", path, rule, detail)
}

func checkOneWorkflow(file CandidateFile, policy *DeployWorkflowPolicy, branches []string) error {
	if !policy.Allows(file.Path) {
		return workflowObjection(file.Path, workflowRuleShape,
			"この名前の workflow は、この納品先が本体に書かせる一覧にありません。")
	}
	if len(file.Content) > MaxDeployWorkflowBytes {
		return workflowObjection(file.Path, workflowRuleShape, "workflow が大きすぎます。")
	}
	// Decoded document by document rather than with Unmarshal, which reads
	// the first and discards the rest in silence. A file whose second
	// document is the one that runs would otherwise be measured against the
	// first.
	decoder := yaml.NewDecoder(strings.NewReader(file.Content))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if strings.TrimSpace(file.Content) == "" {
			// Emptied or deleted. The plan named this file as one the round
			// would build, so an empty one is the round having taken the
			// release path away rather than a file that will not parse —
			// and an objection about YAML would send the next round looking
			// for a syntax error in nothing.
			return workflowObjection(file.Path, workflowRuleShape,
				"この workflow は今回の巡が作るものとして計画に載っています。消さずに、中身のあるファイルとして残してください。")
		}
		return workflowObjection(file.Path, workflowRuleShape, "YAML として読めません。")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return workflowObjection(file.Path, workflowRuleShape, "1 つの YAML 文書だけを書いてください。")
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return workflowObjection(file.Path, workflowRuleShape, "1 つの YAML 文書だけを書いてください。")
	}
	root := document.Content[0]
	if err := refuseUnclassifiableNodes(file.Path, root); err != nil {
		return err
	}
	if root.Kind != yaml.MappingNode {
		return workflowObjection(file.Path, workflowRuleShape, "最上位はキーと値の対応にしてください。")
	}
	// Every expression anywhere in the document, first. A secrets reference
	// hides just as well in an env: value or an if: condition as in a
	// run: step, and a rule that only read the places it expected would be
	// a rule with a hole in exactly the shape of the next idea.
	if err := checkExpressions(file.Path, root, policy); err != nil {
		return err
	}
	top, err := mappingKeys(file.Path, root, workflowTopLevelKeys, workflowRuleShape)
	if err != nil {
		return err
	}
	if trigger, found := top["on"]; found {
		if err := checkTriggers(file.Path, trigger, policy, branches); err != nil {
			return err
		}
	} else {
		return workflowObjection(file.Path, workflowRuleTrigger, "on: が書かれていません。")
	}
	// A workflow with no permissions: of its own is given whatever the
	// repository's default is, which this engine neither sets nor can read.
	// Absent is therefore not "nothing"; it is "unknown", and the whole
	// point of the ceiling is that it is known.
	permissions, found := top["permissions"]
	if !found {
		return workflowObjection(file.Path, workflowRulePermissions,
			"permissions: を最上位に必ず書いてください。書かないとリポジトリの既定の権限が渡ります。")
	}
	if err := checkPermissions(file.Path, permissions, policy); err != nil {
		return err
	}
	jobs, found := top["jobs"]
	if !found {
		return workflowObjection(file.Path, workflowRuleShape, "jobs: が書かれていません。")
	}
	return checkJobs(file.Path, jobs, policy)
}

// refuseUnclassifiableNodes walks the whole document for the constructs the
// rules below cannot see through.
//
// An alias is the sharpest of them: the rules read a node and an alias is a
// second name for a node somewhere else, so a document could pass every
// check reading one copy and run another. A custom tag is the same problem
// one layer down. Neither has any place in a workflow file of a dozen
// lines, so both are refused rather than resolved.
func refuseUnclassifiableNodes(path string, node *yaml.Node) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" {
		return workflowObjection(path, workflowRuleShape,
			"YAML の別名 (& と *) は使わないでください。そのまま読める形で書いてください。")
	}
	switch node.Tag {
	case "", "!!map", "!!seq", "!!str", "!!int", "!!float", "!!bool", "!!null":
	default:
		return workflowObjection(path, workflowRuleShape, "YAML の独自タグは使わないでください。")
	}
	for _, child := range node.Content {
		if err := refuseUnclassifiableNodes(path, child); err != nil {
			return err
		}
	}
	return nil
}

// mappingKeys reads a mapping into a lookup, refusing a key that is not a
// plain string, a key that is not in the allowed set, and a key written
// twice. The last one matters as much as the others: two keys of one name
// mean the file says two things and the reader picks one, and which one is
// whatever the parser happened to decide.
func mappingKeys(path string, node *yaml.Node, allowed map[string]bool, rule string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, workflowObjection(path, rule, "キーと値の対応にしてください。")
	}
	keys := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		key := node.Content[index]
		if key.Kind != yaml.ScalarNode {
			return nil, workflowObjection(path, rule, "キーは文字列で書いてください。")
		}
		name := key.Value
		if _, exists := keys[name]; exists {
			return nil, workflowObjection(path, rule, "同じキーが 2 回書かれています。")
		}
		if allowed != nil && !allowed[name] {
			return nil, workflowObjection(path, rule,
				"この engine が意味を確かめられないキーが書かれています。決まりに挙がっているものだけを書いてください。")
		}
		keys[name] = node.Content[index+1]
	}
	return keys, nil
}

// checkTriggers holds the events that start the workflow to the policy, and
// holds push and pull_request to a branch filter naming this destination's
// own release or integration branch.
func checkTriggers(path string, node *yaml.Node, policy *DeployWorkflowPolicy, branches []string) error {
	events, err := triggerEvents(path, node)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return workflowObjection(path, workflowRuleTrigger, "起動条件が書かれていません。")
	}
	for event, filter := range events {
		if !policy.AllowsTrigger(event) {
			return workflowObjection(path, workflowRuleTrigger,
				"この納品先が許していないイベントで起動しようとしています。")
		}
		if event != WorkflowTriggerPush && event != WorkflowTriggerPullRequest {
			continue
		}
		if err := checkBranchFilter(path, filter, branches); err != nil {
			return err
		}
	}
	return nil
}

// triggerEvents reads the three shapes on: takes — one event, a list of
// events, or a mapping of event to its own settings — into event and
// filter. A nil filter is an event written without one.
func triggerEvents(path string, node *yaml.Node) (map[string]*yaml.Node, error) {
	events := make(map[string]*yaml.Node, 4)
	switch node.Kind {
	case yaml.ScalarNode:
		events[node.Value] = nil
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return nil, workflowObjection(path, workflowRuleTrigger, "起動条件の書き方が読めません。")
			}
			events[child.Value] = nil
		}
	case yaml.MappingNode:
		keys, err := mappingKeys(path, node, nil, workflowRuleTrigger)
		if err != nil {
			return nil, err
		}
		for name, value := range keys {
			events[name] = value
		}
	default:
		return nil, workflowObjection(path, workflowRuleTrigger, "起動条件の書き方が読めません。")
	}
	return events, nil
}

// checkBranchFilter refuses a push or pull_request trigger that is not
// narrowed to this destination's own release or integration branch.
//
// Nothing else will do, and an absent filter least of all. A workflow that
// runs on every push runs on the feature branch this engine pushes to open
// its own pull request — with the repository's secrets, and before a person
// has read a line of it.
func checkBranchFilter(path string, filter *yaml.Node, branches []string) error {
	if filter == nil || filter.Kind == yaml.ScalarNode && filter.Tag == "!!null" {
		return workflowObjection(path, workflowRuleTrigger,
			"push / pull_request は branches でブランチを絞ってください。絞らないと、本体が作業用に push しただけでも走ります。")
	}
	keys, err := mappingKeys(path, filter, nil, workflowRuleTrigger)
	if err != nil {
		return err
	}
	listed, found := keys["branches"]
	if !found {
		return workflowObjection(path, workflowRuleTrigger,
			"push / pull_request には branches を書いてください。branches-ignore や paths だけでは、他のブランチで走るのを止められません。")
	}
	for name := range keys {
		// branches decides WHICH branches start the workflow; paths only
		// narrows that further, and a narrowing cannot widen what branches
		// already settled. Anything else beside it could.
		switch name {
		case "branches", "paths", "paths-ignore":
		default:
			return workflowObjection(path, workflowRuleTrigger,
				"push / pull_request に書けるのは branches と paths だけです。")
		}
	}
	values, err := scalarList(path, listed, workflowRuleTrigger)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return workflowObjection(path, workflowRuleTrigger, "branches が空です。")
	}
	for _, branch := range values {
		if !contains(branches, branch) {
			return workflowObjection(path, workflowRuleTrigger,
				"この納品先のリリース用ブランチ以外が branches に書かれています。")
		}
	}
	return nil
}

// checkPermissions holds a permissions: block to the policy's ceiling. The
// shorthand forms are refused rather than expanded: read-all and write-all
// name every scope there is, including the ones this engine has never heard
// of, which is the opposite of a bounded request.
func checkPermissions(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	if node.Kind == yaml.ScalarNode {
		if node.Tag == "!!null" {
			return workflowObjection(path, workflowRulePermissions,
				"permissions: は空にせず、要る権限だけを書いてください ({} と書けば何も与えません)。")
		}
		return workflowObjection(path, workflowRulePermissions,
			"read-all / write-all のような書き方は使えません。要る権限だけを 1 つずつ書いてください。")
	}
	keys, err := mappingKeys(path, node, nil, workflowRulePermissions)
	if err != nil {
		return err
	}
	for scope, value := range keys {
		if !workflowPermissionScopes[scope] {
			return workflowObjection(path, workflowRulePermissions,
				"この engine が知らない権限の種類が書かれています。")
		}
		if value.Kind != yaml.ScalarNode {
			return workflowObjection(path, workflowRulePermissions, "権限の値は read / write / none のどれかです。")
		}
		level, known := workflowPermissionLevels[value.Value]
		if !known {
			return workflowObjection(path, workflowRulePermissions, "権限の値は read / write / none のどれかです。")
		}
		if level > policy.PermissionCeiling(scope) {
			return workflowObjection(path, workflowRulePermissions,
				"この納品先が許している上限より強い権限を要求しています。")
		}
	}
	return nil
}

func checkJobs(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	jobs, err := mappingKeys(path, node, nil, workflowRuleShape)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return workflowObjection(path, workflowRuleShape, "jobs: が空です。")
	}
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	// Sorted so that a file breaking two rules is refused with the same
	// sentence every time it is read. A refusal that moved between two
	// readings of one file would make a repeated round look like progress.
	sort.Strings(names)
	for _, name := range names {
		if err := checkOneJob(path, jobs[name], policy); err != nil {
			return err
		}
	}
	return nil
}

func checkOneJob(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	job, err := mappingKeys(path, node, workflowJobKeys, workflowRuleShape)
	if err != nil {
		return err
	}
	if condition, found := job["if"]; found {
		if err := checkCondition(path, condition, policy); err != nil {
			return err
		}
	}
	if permissions, found := job["permissions"]; found {
		if err := checkPermissions(path, permissions, policy); err != nil {
			return err
		}
	}
	runsOn, found := job["runs-on"]
	if !found {
		return workflowObjection(path, workflowRuleRunner, "runs-on: が書かれていません。")
	}
	if err := checkRunner(path, runsOn, policy); err != nil {
		return err
	}
	steps, found := job["steps"]
	if !found {
		return workflowObjection(path, workflowRuleShape, "steps: が書かれていません。")
	}
	if steps.Kind != yaml.SequenceNode || len(steps.Content) == 0 {
		return workflowObjection(path, workflowRuleShape, "steps: は手順の並びにしてください。")
	}
	for _, step := range steps.Content {
		if err := checkStep(path, step, policy); err != nil {
			return err
		}
	}
	return nil
}

// checkRunner refuses anything that is not a hosted label the policy names.
//
// A self-hosted runner is a machine the destination owns, usually inside
// its own network with its own standing credentials. A run: step an AI
// wrote executing there is code execution inside the destination's
// infrastructure, which is not a thing a content policy can make safe, so
// it is refused whatever the policy says.
func checkRunner(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	if node.Kind == yaml.MappingNode {
		return workflowObjection(path, workflowRuleRunner,
			"runs-on: に group / labels の指定は使えません。実行環境の名前をそのまま書いてください。")
	}
	labels, err := scalarList(path, node, workflowRuleRunner)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return workflowObjection(path, workflowRuleRunner, "runs-on: が空です。")
	}
	for _, label := range labels {
		if !policy.AllowsRunner(label) {
			return workflowObjection(path, workflowRuleRunner,
				"この納品先が許していない実行環境です。自前の実行機 (self-hosted) は、本体が書いた workflow では使えません。")
		}
	}
	return nil
}

func checkStep(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	step, err := mappingKeys(path, node, workflowStepKeys, workflowRuleShape)
	if err != nil {
		return err
	}
	uses, hasUses := step["uses"]
	run, hasRun := step["run"]
	switch {
	case hasUses == hasRun:
		return workflowObjection(path, workflowRuleShape, "1 つの手順は uses: か run: のどちらか一方です。")
	case hasUses:
		if uses.Kind != yaml.ScalarNode {
			return workflowObjection(path, workflowRuleAction, "uses: は 1 行で書いてください。")
		}
		if !policy.AllowsAction(uses.Value) {
			return workflowObjection(path, workflowRuleAction,
				"この納品先が許していない action です。許されているものを、コミット id まで同じ形で書いてください。")
		}
	case hasRun:
		if run.Kind != yaml.ScalarNode {
			return workflowObjection(path, workflowRuleRunStep, "run: はコマンドの文字列で書いてください。")
		}
		if shell, named := step["shell"]; named {
			if shell.Kind != yaml.ScalarNode || !workflowStepShells[shell.Value] {
				return workflowObjection(path, workflowRuleRunStep,
					"shell: に書けるのは bash か sh だけです。別の言語で走らせると、ここで確かめた内容と実際に走るものが食い違います。")
			}
		}
		if dumpsEnvironment(run.Value) {
			return workflowObjection(path, workflowRuleRunStep,
				"環境変数の一覧を出力する命令が含まれています。実行記録は誰でも読めるので、そこへ資格情報が出ます。")
		}
		if err := refuseUntrustedText(path, run.Value); err != nil {
			return err
		}
	}
	if condition, found := step["if"]; found {
		if err := checkCondition(path, condition, policy); err != nil {
			return err
		}
	}
	return nil
}

// refuseUntrustedText refuses a run: step that pastes something a stranger
// wrote into the script.
//
// A ticket's own words reach this engine as data. A commit message, a
// branch name and a pull request title reach a workflow as text the shell
// expands before it runs: ${{ github.event.head_commit.message }} in a run:
// step is that text becoming commands, on the destination's own account,
// with whatever the job's token can do. The value has to travel as an
// environment variable instead, which is what the objection says.
func refuseUntrustedText(path, script string) error {
	for _, expression := range expressionsIn(script) {
		tokens, err := tokenizeExpression(expression)
		if err != nil {
			return workflowObjection(path, workflowRuleRunStep,
				"run: の ${{ }} に、この engine が意味を確かめられない書き方があります。")
		}
		for index, token := range tokens {
			if token.kind != 'n' || !strings.EqualFold(token.value, "github") ||
				index+2 >= len(tokens) || tokens[index+1].value != "." {
				continue
			}
			switch strings.ToLower(tokens[index+2].value) {
			case "event", "head_ref":
				return workflowObjection(path, workflowRuleRunStep,
					"依頼した人が書いた文字列を run: の中に直接展開しています。"+
						"その文字列はそのまま命令として実行されるので、env: で環境変数に渡してから読んでください。")
			}
		}
	}
	return nil
}

// checkCondition holds an if: to the same secrets rule every other
// expression is held to. An if: takes a bare expression with no ${{ }}
// around it, so the scan that reads those spans never sees one — and
// comparing secrets.SOMETHING against an empty string is a perfectly good
// way to ask whether a secret exists.
//
// A condition may be both at once: a ${{ }} span, then && , then a bare
// comparison against secrets. That is one expression written half inside a
// span and half outside, and reading only the span leaves the other half
// unread — which is a hole the shape of the rule this function exists to
// apply. The spans go to the document-wide scan, the rest comes here, and
// the two together are the whole condition.
func checkCondition(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	if node.Kind != yaml.ScalarNode {
		return workflowObjection(path, workflowRuleShape, "if: は 1 つの条件式で書いてください。")
	}
	bare := withoutExpressionSpans(node.Value)
	if strings.TrimSpace(bare) == "" {
		return nil
	}
	return checkExpression(path, bare, policy)
}

// withoutExpressionSpans is one scalar with its ${{ }} spans taken out, so
// what is left is the part no span scan reads.
//
// Each span becomes a space rather than nothing: "a${{ x }}b" is two
// fragments of a larger expression, and joining them would make one name
// that appears nowhere in the file. An unterminated span is left where it
// is — the scan that owns it refuses the file, and the stray characters
// make this refuse it too rather than reading past them.
func withoutExpressionSpans(text string) string {
	var bare strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "${{")
		if start < 0 {
			bare.WriteString(rest)
			return bare.String()
		}
		end := strings.Index(rest[start+3:], "}}")
		if end < 0 {
			bare.WriteString(rest)
			return bare.String()
		}
		bare.WriteString(rest[:start])
		bare.WriteString(" ")
		rest = rest[start+3+end+2:]
	}
}

// dumpsEnvironment reports whether a shell script prints the environment.
//
// Read command by command rather than by searching the text: "set -euo
// pipefail" is the first line of half the scripts ever written and prints
// nothing, while a bare "set" prints every variable the shell holds.
func dumpsEnvironment(script string) bool {
	for _, line := range strings.Split(script, "\n") {
		for _, command := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ';' || r == '|' || r == '&'
		}) {
			if commandDumpsEnvironment(command) {
				return true
			}
		}
	}
	return false
}

func commandDumpsEnvironment(command string) bool {
	fields := strings.Fields(command)
	// A leading redirection or a subshell's own parenthesis is stripped so
	// that "(set)" and "> log env" read as what they run.
	for len(fields) > 0 && (fields[0] == "(" || fields[0] == "{" || strings.HasPrefix(fields[0], ">")) {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	name := strings.Trim(fields[0], "({")
	switch name {
	case "printenv":
		return true
	case "env":
		// env with a command after it runs that command; env alone, or env
		// with only options, prints everything.
		for _, field := range fields[1:] {
			if !strings.HasPrefix(field, "-") {
				return false
			}
		}
		return true
	case "set":
		// set with no arguments prints every variable; set -e and friends
		// change the shell's behaviour and print nothing.
		return len(fields) == 1
	case "export":
		return len(fields) > 1 && fields[1] == "-p"
	case "declare", "typeset":
		return len(fields) > 1 && (fields[1] == "-p" || fields[1] == "-x")
	}
	return false
}

// scalarList reads a value that may be one scalar or a sequence of them.
func scalarList(path string, node *yaml.Node, rule string) ([]string, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil, nil
		}
		return []string{node.Value}, nil
	case yaml.SequenceNode:
		values := make([]string, 0, len(node.Content))
		for _, child := range node.Content {
			if child.Kind != yaml.ScalarNode {
				return nil, workflowObjection(path, rule, "並びの中は 1 行の値にしてください。")
			}
			values = append(values, child.Value)
		}
		return values, nil
	}
	return nil, workflowObjection(path, rule, "値の書き方が読めません。")
}

// checkExpressions walks every scalar in the document and holds every
// ${{ }} in it to the policy's secrets list.
//
// On the parsed expression rather than on the text. Searching for
// "secrets.NAME" catches the shape somebody writes by hand and misses every
// other way to reach the same place: toJSON(secrets) hands the whole set to
// a step's output, secrets[format('{0}', x)] builds the name at run time,
// and an env: block one job up passes either of them down under a name of
// its own. So the rule is the other way round — the identifier may appear
// only as a direct property of an allowed name, and anything else that
// touches it at all is refused.
func checkExpressions(path string, node *yaml.Node, policy *DeployWorkflowPolicy) error {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode {
		return checkExpressionsIn(path, node.Value, policy)
	}
	for _, child := range node.Content {
		if err := checkExpressions(path, child, policy); err != nil {
			return err
		}
	}
	return nil
}

func checkExpressionsIn(path, text string, policy *DeployWorkflowPolicy) error {
	if strings.Count(text, "${{") != len(expressionsIn(text)) {
		return workflowObjection(path, workflowRuleShape, "${{ が閉じられていません。")
	}
	for _, expression := range expressionsIn(text) {
		if err := checkExpression(path, expression, policy); err != nil {
			return err
		}
	}
	return nil
}

// expressionsIn are the ${{ }} spans of one scalar, in the order written.
func expressionsIn(text string) []string {
	expressions := make([]string, 0, 2)
	rest := text
	for {
		start := strings.Index(rest, "${{")
		if start < 0 {
			return expressions
		}
		rest = rest[start+3:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			return expressions
		}
		expressions = append(expressions, rest[:end])
		rest = rest[end+2:]
	}
}

// expressionToken is one token of a workflow expression.
type expressionToken struct {
	kind  rune // 'n' name, 'p' punctuation or operator, 's' string, '#' number
	value string
}

// checkExpression refuses an expression that touches secrets in any way
// other than reading one allowed name out of it.
func checkExpression(path, expression string, policy *DeployWorkflowPolicy) error {
	tokens, err := tokenizeExpression(expression)
	if err != nil {
		return workflowObjection(path, workflowRuleShape,
			"${{ }} の中に、この engine が意味を確かめられない書き方があります。")
	}
	for index, token := range tokens {
		if token.kind != 'n' || !strings.EqualFold(token.value, "secrets") {
			continue
		}
		// A property OF something else that happens to be called secrets is
		// refused too. It is not the secrets context and means nothing in a
		// workflow, so the only thing it can be is an attempt to read like
		// one thing and run as another.
		if index > 0 && tokens[index-1].kind == 'p' && tokens[index-1].value == "." {
			return workflowObjection(path, workflowRuleSecrets,
				"secrets の読み方がこの engine の許す形ではありません。")
		}
		if index+2 >= len(tokens) || tokens[index+1].kind != 'p' || tokens[index+1].value != "." ||
			tokens[index+2].kind != 'n' {
			return workflowObjection(path, workflowRuleSecrets,
				"secrets はひとつずつ名前を書いて読んでください。secrets 全体を渡す書き方や、名前を組み立てる書き方は使えません。")
		}
		if !policy.AllowsSecret(tokens[index+2].value) {
			return workflowObjection(path, workflowRuleSecrets,
				"この納品先が許していない secret を読もうとしています。")
		}
	}
	return nil
}

// tokenizeExpression splits a workflow expression into the tokens the rule
// above reads. A character it does not know is an error rather than a
// token: an expression this cannot take apart is one whose secrets use it
// cannot see, and that has to read as a refusal.
func tokenizeExpression(expression string) ([]expressionToken, error) {
	tokens := make([]expressionToken, 0, 16)
	runes := []rune(expression)
	for index := 0; index < len(runes); {
		character := runes[index]
		switch {
		case character == ' ' || character == '\t' || character == '\n' || character == '\r':
			index++
		case character == '\'':
			// A single-quoted string; '' is an escaped quote inside one.
			end := index + 1
			for end < len(runes) {
				if runes[end] == '\'' {
					if end+1 < len(runes) && runes[end+1] == '\'' {
						end += 2
						continue
					}
					break
				}
				end++
			}
			if end >= len(runes) {
				return nil, errors.New("unterminated string")
			}
			tokens = append(tokens, expressionToken{kind: 's', value: string(runes[index+1 : end])})
			index = end + 1
		case character >= '0' && character <= '9':
			end := index
			for end < len(runes) && (runes[end] >= '0' && runes[end] <= '9' || runes[end] == '.' ||
				runes[end] == 'e' || runes[end] == 'E' || runes[end] == 'x' ||
				runes[end] >= 'a' && runes[end] <= 'f' || runes[end] >= 'A' && runes[end] <= 'F') {
				end++
			}
			tokens = append(tokens, expressionToken{kind: '#', value: string(runes[index:end])})
			index = end
		case character == '_' || character == '-' || character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z':
			end := index
			for end < len(runes) && (runes[end] == '_' || runes[end] == '-' ||
				runes[end] >= 'a' && runes[end] <= 'z' || runes[end] >= 'A' && runes[end] <= 'Z' ||
				runes[end] >= '0' && runes[end] <= '9') {
				end++
			}
			tokens = append(tokens, expressionToken{kind: 'n', value: string(runes[index:end])})
			index = end
		case strings.ContainsRune(".[](),*+-/%", character):
			tokens = append(tokens, expressionToken{kind: 'p', value: string(character)})
			index++
		case strings.ContainsRune("!<>=&|", character):
			end := index
			for end < len(runes) && strings.ContainsRune("!<>=&|", runes[end]) {
				end++
			}
			tokens = append(tokens, expressionToken{kind: 'p', value: string(runes[index:end])})
			index = end
		default:
			return nil, errors.New("unknown character in expression")
		}
	}
	return tokens, nil
}
