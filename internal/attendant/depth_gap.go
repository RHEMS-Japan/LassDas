package attendant

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"automation.internal/ticket-ingress/internal/runtime"
	"automation.internal/ticket-ingress/internal/state"
	"automation.internal/ticket-ingress/internal/worker"
)

// The release path a destination does not have.
//
// A destination whose setting says production needs a way to get there:
// something that deploys, something that records what landed, somewhere the
// change can be looked at afterwards. The depth record already notices when
// that is not configured and delivers as far as the proposal, with a line
// on the ticket naming the settings somebody would have to write.
//
// Naming them is asking a person to do the work. What is missing is turned
// into work for the round that is about to run instead: the implementing
// round builds the part of the path that lives in the destination's own
// repository, inside the scope that repository declared writable, and it
// goes out in the same pull request as the request itself. Nothing is asked
// of anyone.
//
// What the engine was not handed the means to apply is named rather than
// asked for. Two things it never has: a file whose directory begins with a
// dot — the whole path vocabulary refuses one (internal/worker/config.go's
// relative path pattern), so the deploy workflow itself is out of reach —
// and the instance's own settings, which say what this engine is allowed to
// run and are not the requester's to change. Those are reported by name,
// after the delivery, as parts it did not apply.

// releasePathBuildDeployment and releasePathBuildObservation are the two
// parts of the path the engine builds itself. They are named for the report
// reader rather than by a settings key, because a part the engine applies
// never appears in the report as something that was left undone.
const (
	releasePathBuildDeployment  = "デプロイ工程のうちリポジトリの中で動く部分"
	releasePathBuildObservation = "反映を画面から確かめる入口"
)

// consumerReleaseSettings is the destination's release configuration, read
// the lenient way every read in this package is read: the few fields this
// needs rather than the whole typed structure, so a plan can still be made
// for a destination whose configuration this engine cannot fully parse.
type consumerReleaseSettings struct {
	// The two branches a deploy workflow may filter on. They are the
	// destination's own, already written down and already validated, so
	// the workflow policy does not name them a second time.
	IntegrationBranch   string   `json:"integration_branch"`
	ReleaseBranch       string   `json:"release_branch"`
	StagingOrigin       string   `json:"staging_origin"`
	ProductionOrigin    string   `json:"production_origin"`
	StagingWorkflow     string   `json:"staging_workflow"`
	ProductionWorkflow  string   `json:"production_workflow"`
	StagingLoginURL     string   `json:"staging_login_url"`
	ProductionLoginURL  string   `json:"production_login_url"`
	ObservationLanguage string   `json:"observation_language"`
	Scope               []string `json:"-"`
	GitHub              struct {
		StagingWorkflow struct {
			Path string `json:"path"`
		} `json:"staging_workflow"`
		ProductionWorkflows []struct {
			Path string `json:"path"`
		} `json:"production_workflows"`
		StagingDigestCommit *struct {
			ExactMessagePrefix string `json:"exact_message_prefix"`
		} `json:"staging_digest_commit"`
	} `json:"github_contract"`
	Mode struct {
		AllowedFilePrefixes []string `json:"allowed_file_prefixes"`
	} `json:"mode"`
	// Infrastructure carries the one means this file acts on: the content
	// policy for deploy workflows. Read leniently like everything else
	// here, and typed strictly once it is read, because what it permits is
	// the only hole in the engine's path vocabulary.
	Infrastructure struct {
		DeployWorkflows *worker.DeployWorkflowPolicy `json:"deploy_workflows"`
	} `json:"infrastructure"`
}

// workflowPolicy is the destination's handed means for authoring deploy
// workflows, or nil when it was not handed or does not hold together. A
// policy that would be refused when the configuration loads is treated as
// absent here: this read is the lenient one, and a lenient read must not be
// the way a refused policy takes effect.
func (s consumerReleaseSettings) workflowPolicy() *worker.DeployWorkflowPolicy {
	policy := s.Infrastructure.DeployWorkflows
	if policy == nil || worker.ValidateDeployWorkflowPolicy(*policy) != nil {
		return nil
	}
	return policy
}

// errDestinationGone is the destination not being in the configuration at
// all, which is an operator's edit rather than a fault. It is told apart
// from a configuration that cannot be read because the two want opposite
// answers: an edit that removed a destination mid-delivery must stop the
// promotion, and a file that could not be opened this second must not.
var errDestinationGone = errors.New("repository is not a configured consumer")

// readConsumerReleaseSettings finds one destination's release configuration.
func readConsumerReleaseSettings(consumerConfigPath, repository string) (consumerReleaseSettings, error) {
	raw, err := os.ReadFile(consumerConfigPath)
	if err != nil {
		return consumerReleaseSettings{}, errors.New("consumer config unreadable")
	}
	var parsed struct {
		Consumers []struct {
			Repository string `json:"repository"`
			consumerReleaseSettings
		} `json:"consumers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return consumerReleaseSettings{}, errors.New("consumer config unreadable")
	}
	for _, consumer := range parsed.Consumers {
		if consumer.Repository == repository {
			settings := consumer.consumerReleaseSettings
			settings.Scope = settings.Mode.AllowedFilePrefixes
			return settings, nil
		}
	}
	return consumerReleaseSettings{}, errDestinationGone
}

// stagingWorkflowPath and productionWorkflowPath are where the workflow that
// deploys is expected to be found in the destination's repository. The
// contract's own path wins; the short filename is where a destination that
// named only that keeps it, which is the conventional place.
func (s consumerReleaseSettings) stagingWorkflowPath() string {
	return workflowPath(s.GitHub.StagingWorkflow.Path, s.StagingWorkflow)
}

func (s consumerReleaseSettings) productionWorkflowPath() string {
	if len(s.GitHub.ProductionWorkflows) > 0 {
		return workflowPath(s.GitHub.ProductionWorkflows[0].Path, s.ProductionWorkflow)
	}
	return workflowPath("", s.ProductionWorkflow)
}

// workflowMeans says why the engine cannot write one workflow file. Two
// different reasons, kept apart because the report names them to whoever
// can act on them: a path whose first directory begins with a dot is out of
// reach of the whole engine and no setting changes that, while a path the
// destination simply did not declare writable is that destination's own
// choice and one line of configuration away.
func workflowMeans(name string, scope []string, policy *worker.DeployWorkflowPolicy) string {
	if policy.Allows(name) {
		// Handed. The destination wrote down what such a file may contain,
		// and this is one of the names it wrote down, so the round builds
		// it and the content gate holds what comes back to that policy.
		return ""
	}
	if hiddenDirectory(name) {
		if policy != nil {
			return "この納品先が本体に書かせる workflow (infrastructure.deploy_workflows.paths) にこの名前がありません。"
		}
		return "先頭がドットのディレクトリの中は本体が書けません。"
	}
	if !withinWritableScope(name, scope) {
		return "この場所は変更してよい範囲 (mode.allowed_file_prefixes) に入っていません。"
	}
	return ""
}

// hiddenDirectory reports whether any directory in a path begins with a
// dot, which is what the engine's path vocabulary refuses.
func hiddenDirectory(name string) bool {
	for _, element := range strings.Split(path.Clean(name), "/") {
		if strings.HasPrefix(element, ".") {
			return true
		}
	}
	return false
}

func workflowPath(contractPath, filename string) string {
	if contractPath != "" {
		return contractPath
	}
	if filename == "" {
		return ""
	}
	return ".github/workflows/" + filename
}

// detectReleasePathGap works out what one destination's release path is
// missing, before the round that could build some of it runs.
//
// It reads the settings and the destination's own checked-out tree, and
// nothing else: no network, no model. A destination that stops at the
// proposal has no path to build and gets an empty plan, and so does one
// whose path is already complete — which is the whole point of reading the
// tree rather than the settings alone. A workflow the settings name and the
// repository has is a path that works.
func detectReleasePathGap(config runtime.Config, run state.RunOverview, runDir string) (worker.ReleasePathPlan, error) {
	repository, err := readField(runDir, "ticket-draft.json", "repository")
	if err != nil {
		return worker.ReleasePathPlan{}, errors.New("run repository unreadable")
	}
	configured, err := consumerDelivery(config.ConsumerConfigPath, repository)
	if err != nil {
		return worker.ReleasePathPlan{}, err
	}
	plan := worker.ReleasePathPlan{
		SchemaVersion: worker.ReleasePathSchemaVersion,
		Repository:    repository, Configured: configured, DecidedAt: time.Now().UTC(),
	}
	if !worker.Delivery(configured).ReachesIntegration() {
		// The destination stops at the proposal. There is no path past it
		// to be missing, and a round told to build one would be building
		// something nobody asked for.
		return plan, nil
	}
	settings, err := readConsumerReleaseSettings(config.ConsumerConfigPath, repository)
	if err != nil {
		return worker.ReleasePathPlan{}, err
	}
	tree := filepath.Join(runDir, "target-repo")
	production := worker.Delivery(configured).ReachesProduction()

	plan.Items = append(plan.Items, missingWorkflowItems(settings, tree, production)...)
	plan.Items = append(plan.Items, missingObservationItems(settings, production)...)
	plan.Items = append(plan.Items, missingCardItems(config, run)...)
	// The digest-commit policy is the engine's own to write when the engine
	// wrote the workflow that makes the commit: the shape of that commit is
	// whatever the file it just authored produces, so there is nothing to
	// observe and nobody to ask. Where the means was not handed, somebody
	// else's workflow makes that commit and the engine reports the setting
	// by name instead.
	if production && settings.GitHub.StagingDigestCommit == nil && settings.workflowPolicy() == nil {
		plan.Items = append(plan.Items, worker.ReleasePathItem{
			Name: "github_contract.staging_digest_commit", Kind: worker.ReleasePathDigestCommit,
			Detail: "staging へ反映された内容を記録するコミットの形 (メッセージの接頭辞・対象パス・実行者) の申告です。" +
				"本番への昇格は、この申告と実際のコミットを突き合わせて「動いたのはこの変更だ」と確かめます。",
			Means: "本体が観測できるのは反映されたパスだけで、メッセージの接頭辞と実行者は観測できません。" +
				"観測していない値を設定に書くことはしません。",
		})
	}
	if len(plan.Items) == 0 {
		return plan, nil
	}
	// A part whose means this instance was handed is a part the engine
	// applies itself, so it leaves the report's list and joins the round's
	// work. Nothing is handed today, which is why every unapplied part
	// below is still unapplied; the day one is, this is where it changes.
	handed := handedMeans(config)
	for index, item := range plan.Items {
		if item.Means != "" && slices.Contains(handed, item.Means) {
			plan.Items[index].Means = ""
		}
	}
	plan.Items = append(plan.Items, buildableReleasePathItems(settings, production)...)
	if len(plan.Items) > worker.MaxReleasePathItems {
		plan.Items = plan.Items[:worker.MaxReleasePathItems]
	}
	// The files the round is allowed to create outside the writable scope.
	// Named here, in the record the round is rendered from, because this is
	// the only list the path gates admit: a file the policy permits and
	// this plan does not name is refused exactly like any other dotted
	// path.
	plan.WorkflowFiles = plannedWorkflowFiles(plan, settings)
	plan.Instruction = releasePathInstruction(plan, settings)
	return plan, nil
}

// plannedWorkflowFiles are the workflow files this plan says the round will
// create: the ones the destination's policy names, that the repository does
// not have, in the order they were found.
func plannedWorkflowFiles(plan worker.ReleasePathPlan, settings consumerReleaseSettings) []string {
	policy := settings.workflowPolicy()
	if policy == nil {
		return nil
	}
	files := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		if item.Kind == worker.ReleasePathWorkflow && item.Buildable() && policy.Allows(item.Name) &&
			!slices.Contains(files, item.Name) {
			files = append(files, item.Name)
		}
	}
	if len(files) == 0 {
		return nil
	}
	return files
}

// missingWorkflowItems names the deploy workflows the settings expect and
// the repository does not have.
//
// Buildable only where the destination handed the means. A path whose
// first directory begins with a dot is not addressable by anything in the
// engine — the candidate refuses one outright rather than skipping it — so
// a workflow file is out of reach whatever a destination declares writable.
// The one way in is a content policy: a destination that wrote down what
// such a file may contain, and named this file in it, gets it built.
func missingWorkflowItems(settings consumerReleaseSettings, tree string, production bool) []worker.ReleasePathItem {
	policy := settings.workflowPolicy()
	var items []worker.ReleasePathItem
	add := func(name, environment string) {
		if name == "" {
			items = append(items, worker.ReleasePathItem{
				Name: environment + "_workflow", Kind: worker.ReleasePathWorkflow,
				Detail: environment + " へ反映する workflow の名前が設定にありません。",
				Means:  "何がこの納品先を反映しているかは本体からは分かりません。",
			})
			return
		}
		if _, err := os.Stat(filepath.Join(tree, filepath.FromSlash(name))); err == nil {
			return
		}
		items = append(items, worker.ReleasePathItem{
			Name: name, Kind: worker.ReleasePathWorkflow,
			Detail: environment + " へ反映する workflow がリポジトリにありません。",
			Means:  workflowMeans(name, settings.Scope, policy),
		})
	}
	add(settings.stagingWorkflowPath(), "staging")
	if production {
		add(settings.productionWorkflowPath(), "production")
	}
	return items
}

// missingObservationItems names the settings the observing browser needs and
// does not have. They are values about the destination's own environments,
// so the engine names them and invents none of them.
func missingObservationItems(settings consumerReleaseSettings, production bool) []worker.ReleasePathItem {
	entries := []struct{ name, value, detail string }{
		{"staging_origin", settings.StagingOrigin, "staging が応答する場所です。"},
	}
	if production {
		entries = append(entries,
			struct{ name, value, detail string }{"production_origin", settings.ProductionOrigin, "本番が応答する場所です。"},
			struct{ name, value, detail string }{"production_login_url", settings.ProductionLoginURL,
				"本番の画面にサインインが要るときの入口です。サインインの要らない画面なら空のままで構いません。"},
		)
	}
	entries = append(entries,
		struct{ name, value, detail string }{"observation_language", settings.ObservationLanguage,
			"確認の browser が画面に求める言語です。"},
	)
	var items []worker.ReleasePathItem
	for _, entry := range entries {
		if entry.value != "" {
			continue
		}
		items = append(items, worker.ReleasePathItem{
			Name: entry.name, Kind: worker.ReleasePathOrigin, Detail: entry.detail,
			Means: "納品先の環境そのものの値なので、本体には決められません。",
		})
	}
	return items
}

// missingCardItems names the instance-side settings that give a delivery its
// cards. They say what this engine is allowed to run, so they are the
// operator's and never the requester's — the same three keys the depth
// record already names, kept in one place so the two cannot drift.
func missingCardItems(config runtime.Config, run state.RunOverview) []worker.ReleasePathItem {
	means := "本体が自分の実行権限を書き換えることはしません。"
	if !config.Chain.Deliver.Enabled() {
		var items []worker.ReleasePathItem
		for _, key := range []string{
			"chain.deliver.checks_profile", "chain.deliver.integrate_profile", "chain.deliver.promote_profile",
		} {
			items = append(items, worker.ReleasePathItem{
				Name: key, Kind: worker.ReleasePathCards,
				Detail: "納品を運ぶカードの設定です。これが無いと、変更は Pull Request までで止まります。",
				Means:  means,
			})
		}
		return items
	}
	if !deliverConfigured(config.Chain, run) {
		return []worker.ReleasePathItem{{
			Name: "chain.deliver.enabled_after", Kind: worker.ReleasePathCards,
			Detail: "この依頼は、納品のカードが有効になった時刻より前に受け付けられています。",
			Means:  means,
		}}
	}
	return nil
}

// buildableReleasePathItems is the part of the path the engine builds
// itself: what lives in the destination's own repository, inside the scope
// that repository declared writable. It goes out in the same pull request
// as the request, so the destination gets the change and the way to deploy
// and see it in one place.
func buildableReleasePathItems(settings consumerReleaseSettings, production bool) []worker.ReleasePathItem {
	where := "本番と staging"
	if !production {
		where = "staging"
	}
	deployment := where + " へ反映するために、リポジトリの中で動く部分 (適用するマニフェスト、起動スクリプト、設定の差し替え) を" +
		"変更してよい場所の下に用意してください。これを呼び出す workflow ファイル自体は本体が書けないので、" +
		"どう呼び出すかを、変更してよい場所の下の文書に 1 か所だけ書き残してください。"
	if settings.workflowPolicy() != nil {
		// The means is handed, so the part that used to be left as a note
		// in a document is the work itself. The one sentence changes with
		// it: an instruction that asked for a workflow and told the writer
		// it could not write one would be answered by doing neither.
		deployment = where + " へ反映するために、リポジトリの中で動く部分 (適用するマニフェスト、起動スクリプト、設定の差し替え) を" +
			"変更してよい場所の下に用意し、それを呼び出す workflow ファイルを下の一覧の名前で作ってください。"
	}
	return []worker.ReleasePathItem{
		{
			Name: releasePathBuildDeployment, Kind: worker.ReleasePathWorkflow,
			Detail: deployment,
		},
		{
			Name: releasePathBuildObservation, Kind: worker.ReleasePathObservation,
			Detail: "反映された内容を画面から確かめられる入口を用意してください。" +
				"依頼が画面の文言を約束しているなら、その文言が出る画面がそれに当たります。" +
				"約束していないなら、動いている版が分かる表示を 1 か所に出してください。",
		},
	}
}

// releasePathInstruction is what the implementing round is told. It carries
// the parts the engine builds and nothing else: an item the engine cannot
// apply is not work anyone in this round can do, and putting it here would
// turn the round into a report about configuration.
func releasePathInstruction(plan worker.ReleasePathPlan, settings consumerReleaseSettings) string {
	buildable := plan.Buildable()
	if len(buildable) == 0 {
		return ""
	}
	lines := []string{
		"### この納品先にはまだリリース経路がありません (依頼と同じ変更で作ってください)",
		"- この納品先は " + plan.Configured + " まで届ける設定ですが、そこへ届く経路がまだリポジトリにありません。" +
			"経路を作ることも今回の依頼の一部として扱ってください。依頼の本文にその指示が無くても同じです。",
		"- 作ったものは依頼の変更と同じ Pull Request に入ります。別の課題に分けないでください。",
	}
	for _, item := range buildable {
		lines = append(lines, "- "+item.Name+": "+item.Detail)
	}
	if len(settings.Scope) > 0 {
		lines = append(lines,
			"- 置ける場所は "+strings.Join(settings.Scope, " / ")+" の下だけです。"+
				"ここに置けないものは作らず、最後の報告に「置けなかったもの」として 1 行で書いてください。")
	}
	lines = append(lines, workflowInstructionLines(plan, settings)...)
	lines = append(lines,
		"- 資格情報・権限設定・利用上限には触れないでください。経路のうちそれらが要る部分は、本体が別に記録します。",
		"- 経路を作ったことで依頼そのものが変わることはありません。依頼を満たす変更を先に済ませてください。")
	return worker.BoundedReleasePathInstruction(strings.Join(lines, "\n"))
}

// workflowInstructionLines are what the round is told about the workflow
// files it may create, and what they may contain.
//
// The policy is quoted rather than summarised. It is checked again, to the
// letter, before the change is sealed, so a round that is told less than
// the rule it will be held to is a round that gets refused for a reason it
// was never given — and the objection that comes back names the rule, which
// only helps somebody who was told the rule existed.
func workflowInstructionLines(plan worker.ReleasePathPlan, settings consumerReleaseSettings) []string {
	policy := settings.workflowPolicy()
	if policy == nil || len(plan.WorkflowFiles) == 0 {
		return nil
	}
	branches := make([]string, 0, 2)
	for _, branch := range []string{settings.ReleaseBranch, settings.IntegrationBranch} {
		if branch != "" && !slices.Contains(branches, branch) {
			branches = append(branches, branch)
		}
	}
	lines := []string{
		"- 作ってよい workflow ファイルは次の名前だけです: " + strings.Join(plan.WorkflowFiles, " / ") +
			"。この名前以外の場所に .github の下のファイルを作ると、その実行は丸ごと破棄されます。",
		"- workflow の中身は次の範囲に収めてください。範囲を外れたものは封じる前に機械的に拒否され、次の巡でやり直しになります。",
		"  - on: に書いてよいイベント: " + strings.Join(policy.Triggers, " / ") +
			"。push と pull_request は branches でブランチを絞った場合だけ使えます" + branchesPhrase(branches) + "。",
		"  - permissions: は job ごとにも全体にも必ず書いてください。書かなかった場合は拒否されます。書いてよい上限は " +
			permissionsPhrase(policy) + " です。",
		"  - ${{ }} の中で secrets に触れてよいのは " + secretsPhrase(policy) +
			" だけです。toJSON(secrets) のように secrets 全体を渡す書き方は使えません。",
		"  - uses: に書いてよいのは " + actionsPhrase(policy) + " だけです (この形のまま、コミット id も含めて一致すること)。",
		"  - runs-on: に書いてよいのは " + strings.Join(policy.Runners, " / ") + " だけです。",
		"  - run: の中で環境変数の一覧を出力しないでください (env / printenv / set / export -p)。",
	}
	return lines
}

// branchesPhrase names the branches a push or pull_request filter may hold,
// or says nothing when the destination named none — in which case no branch
// filter can pass and the trigger is unusable, which the gate says at the
// time rather than this predicting it.
func branchesPhrase(branches []string) string {
	if len(branches) == 0 {
		return ""
	}
	return " (絞ってよいのは " + strings.Join(branches, " / ") + " だけです)"
}

// permissionsPhrase writes the ceiling out scope by scope, in a fixed order
// so two renderings of one policy read the same.
func permissionsPhrase(policy *worker.DeployWorkflowPolicy) string {
	scopes := make([]string, 0, len(policy.Permissions))
	for scope := range policy.Permissions {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	if len(scopes) == 0 {
		return "permissions: {} (何も与えない)"
	}
	phrases := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		phrases = append(phrases, scope+": "+policy.Permissions[scope])
	}
	return strings.Join(phrases, " / ")
}

func secretsPhrase(policy *worker.DeployWorkflowPolicy) string {
	if len(policy.Secrets) == 0 {
		return "なし (secrets には一切触れない)"
	}
	names := make([]string, 0, len(policy.Secrets))
	for _, name := range policy.Secrets {
		names = append(names, "secrets."+name)
	}
	return strings.Join(names, " / ")
}

func actionsPhrase(policy *worker.DeployWorkflowPolicy) string {
	if len(policy.Actions) == 0 {
		return "なし (uses: を使わず run: だけで書く)"
	}
	return strings.Join(policy.Actions, " / ")
}

// releasePathUnapplied is the names of the parts the engine did not apply,
// for the report to say what the delivery left undone.
//
// Names only, and never a sentence asking for them. The report says what it
// did and what it did not; a line telling a person to go and configure
// something is the shape this whole file exists to remove. The outcome
// section reads this.
func releasePathUnapplied(plan worker.ReleasePathPlan) []string {
	return plan.UnappliedNames()
}

// handedMeans is what this instance was given to work with beyond the
// destination's own repository.
//
// Empty today: the only credential the engine holds is the destination's
// own token, which reaches the publish and validate cards and nothing else.
// The means an operator can hand it — the credentials a stage may carry,
// the resources it may create — arrive with the configuration that declares
// them, and this reads them when it does. Reception then asks once, as a
// missing means, for whatever a destination's path needs and this does not
// carry.
func handedMeans(runtime.Config) []string { return nil }

// sealReleasePathPlan writes the plan down, once per round it is rendered
// for. Best-effort by design, like the depth record: what is lost when the
// volume refuses the write is the round's extra work, not the round.
func sealReleasePathPlan(runDir string, plan worker.ReleasePathPlan, logger Logger) {
	if plan.Seal() != nil {
		return
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return
	}
	path := releasePathPlanFile(runDir)
	// Removed before the write for the reason every other record here is: a
	// link left at the path must not carry the write somewhere else.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		logger.Error("the release path plan could not be recorded; the round continues",
			"repository", plan.Repository, "error", err.Error())
	}
}

// readReleasePathPlan reads the sealed plan back. A record that is not ours
// reads as no plan at all, which is the same state as a destination whose
// path is complete.
func readReleasePathPlan(runDir string) (worker.ReleasePathPlan, bool) {
	plan, err := worker.ReadReleasePathFile(releasePathPlanFile(runDir))
	if err != nil {
		return worker.ReleasePathPlan{}, false
	}
	return plan, true
}

// releasePathPlanFile is the path the round's instruction is rendered from,
// for the stage that passes it to the command.
func releasePathPlanFile(runDir string) string {
	return filepath.Join(runDir, worker.ReleasePathFile)
}

// holdReleasePath records why a delivery that reached staging did not go on
// to production, so the report can say it in the requester's own terms
// rather than leaving the delivery looking like it simply stopped.
func holdReleasePath(runDir, reason string, logger Logger) {
	plan, ok := readReleasePathPlan(runDir)
	if !ok {
		// No plan, which is the ordinary state of a destination whose path
		// was complete when the delivery was claimed. Something about it
		// stopped being true before the promotion, and the reason still has
		// to reach the requester — so the record starts here rather than
		// being skipped. Skipped, the report fell through to the sentence
		// about waiting for an operator to look, which is a different thing
		// that did not happen: the requester was told to go and approve a
		// promotion that nothing was waiting on.
		repository, err := readField(runDir, "ticket-draft.json", "repository")
		if err != nil || repository == "" {
			logger.Error("the delivery stopped before production and the reason could not be recorded",
				"reason", reason)
			return
		}
		plan = worker.ReleasePathPlan{
			SchemaVersion: worker.ReleasePathSchemaVersion,
			Repository:    repository, DecidedAt: time.Now().UTC(),
		}
		if depth, sealed := readDepthRecord(runDir); sealed {
			plan.Configured = depth.Configured
		}
	}
	if plan.Hold == reason {
		return
	}
	plan.Hold = reason
	sealReleasePathPlan(runDir, plan, logger)
}

// releasePathHold reads that reason back.
func releasePathHold(runDir string) string {
	plan, ok := readReleasePathPlan(runDir)
	if !ok {
		return ""
	}
	return plan.Hold
}

// withinWritableScope reports whether a repository path is somewhere the
// destination declared the engine may write. Kept here rather than borrowed
// from the worker's own matcher because this read is the lenient one: a
// configuration this cannot fully parse still has to yield an answer.
func withinWritableScope(name string, scope []string) bool {
	clean := path.Clean(name)
	for _, prefix := range scope {
		if clean == prefix || strings.HasSuffix(prefix, "/") && strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}
