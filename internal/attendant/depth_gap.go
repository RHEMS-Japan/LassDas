package attendant

import (
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
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
}

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
	return consumerReleaseSettings{}, errors.New("repository is not a configured consumer")
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
func workflowMeans(name string, scope []string) string {
	if hiddenDirectory(name) {
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
	if production && settings.GitHub.StagingDigestCommit == nil {
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
	plan.Instruction = releasePathInstruction(plan, settings)
	return plan, nil
}

// missingWorkflowItems names the deploy workflows the settings expect and
// the repository does not have.
//
// Never buildable, and not because of this destination. A path whose first
// directory begins with a dot is not addressable by anything in the engine
// — the candidate refuses one outright rather than skipping it — so the
// workflow file is the one part of the path that is always out of reach,
// whatever a destination declares writable.
func missingWorkflowItems(settings consumerReleaseSettings, tree string, production bool) []worker.ReleasePathItem {
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
			Means:  workflowMeans(name, settings.Scope),
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
	return []worker.ReleasePathItem{
		{
			Name: releasePathBuildDeployment, Kind: worker.ReleasePathWorkflow,
			Detail: where + " へ反映するために、リポジトリの中で動く部分 (適用するマニフェスト、起動スクリプト、設定の差し替え) を" +
				"変更してよい場所の下に用意してください。これを呼び出す workflow ファイル自体は本体が書けないので、" +
				"どう呼び出すかを、変更してよい場所の下の文書に 1 か所だけ書き残してください。",
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
	lines = append(lines,
		"- 資格情報・権限設定・利用上限には触れないでください。経路のうちそれらが要る部分は、本体が別に記録します。",
		"- 経路を作ったことで依頼そのものが変わることはありません。依頼を満たす変更を先に済ませてください。")
	return worker.BoundedReleasePathInstruction(strings.Join(lines, "\n"))
}

// releasePathUnapplied is the names of the parts the engine did not apply,
// for the report to say what the delivery left undone.
//
// Names only, and never a sentence asking for them. The report says what it
// did and what it did not; a line telling a person to go and configure
// something is the shape this whole file exists to remove. The outcome
// section reads this.
func releasePathUnapplied(plan worker.ReleasePathPlan) []string {
	names := make([]string, 0, len(plan.Items))
	for _, item := range plan.Unapplied() {
		names = append(names, item.Name)
	}
	return names
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
	if !ok || plan.Hold == reason {
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
