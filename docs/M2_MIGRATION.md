# M2 移行設計: 進行と実行の Hermes 純正化 (v3 draft 2026-08-21)

対象: docs/TARGET_SHAPE.md (確定済みの最終形) への移行。本書はその実施設計 — 何を・どの順で・どこまでやったら完了か。

## 変更履歴

- v3 (2026-08-21): 再評価 (Gate 1 FAIL・61 点) のブロッカー 5 件と推奨 14 件を反映。①visiblecheck を「残す」へ訂正 (納品ゲートが現用 — `cmd/controller/commands.go:376,417`) ②行数目標を算術成立する実測値へ再訂正 (削減 ~5,000 行 → 到達 ~27,200 行) ③「実装も審判も同一プログラム hermes」と既存検証 `config.go:70-72` の衝突を解消する再設計を明記 ④カバレッジ DoD の対象を実名 (`internal/runtime`) へ ⑤direct-command カードの失敗が「再試行なしで blocked」になる実機挙動に運用前提と無人完走 DoD を合わせた ⑥骨格図の依存の向き・終端報告の読み取り元・プロファイル一覧・seal-candidate 仕様・Dockerfile/entrypoint 改訂・ロールバック判定者を追記
- v2 (2026-08-21): 初回評価 (FAIL・58 点) の反映 — レビュー実行の関所所有 / lane 撤回 / 処遇表新設
- v1 (2026-08-21): 初版

## 概要

現行 (M1) は「受付・進行・実行・関所」を全て自前 Go が駆動し、実行だけ claude/codex CLI を外部呼び出しする。M2 では**進行を Hermes 純正カンバンに、実装の実行を Hermes 純正エージェントに移し、自前 Go は関所 (policy kernel) に縮める**。ハーネスは 1 本になり、CLI 同梱・版追従・自前進行配線 (テスト 0% の 1,272 行) が消える。

## 背景と根拠 (要約)

- 維持/廃止評価 + 独立 2 系統クロスチェックの帰結 ([issue #9](https://github.com/RHEMS-Japan/LassDas/issues/9)): 実失敗 3 件 (606/608/610) は全て自前進行配線で発生。価値は関所に集中
- 発注者決定 (2026-08-21): 道 2 直行 / 実行も Hermes 純正 (一番シンプルな形から始め、不足したら分離)
- 608 型の非収束対策 (巡別レンズ・過去巡の受け渡し) は関所がレビュー起動時に注入する (レンズ注入・レビュアー配列・指名起動は既存実装が想定より進んでいる — 実在確認済み: `config.Models.Reviewers` 配列 + 2 ベンダー強制 `config.go:554-575`、`--reviewer` 指名 `cmd/worker/agent.go:155`、Lens 注入 `model.go:439` と成果物束縛 `artifact.go:369,388`。**新規に要るのは過去巡 findings のレビュー側注入のみ**)

## 設計

### 信頼境界 (核心)

役割を「作る側」と「判定する側」で分け、**判定する側の実行は関所が所有し続ける**:

- **実装役 = 作る側 = 非信頼**。Hermes 純正エージェントが自由に作業する。成果物の確定は関所の封緘 verb が行い、変更検出は `git status --porcelain` の観測 (自己申告ではない・範囲外書き込みはエラー — 既存実装 `internal/worker/agent.go:206-225` の流用)。作る側に証明は不要 — レビューと検証が判定するため
- **レビュー役・検証 = 判定する側 = 関所の所有物**。レビューは関所自身が子プロセスとして起動する (`agent-review` 系譜・起動構造は実在 `internal/worker/agent.go:124-135`)。「どの接続先のどのモデルが・どの digest の成果物を読んだか」は関所自身の観測。レビュー後のツリー照合 (`agent_review.go:45-72`) も現行のまま
- ingest 型 (外部レビューの持ち込み) の verb は作らない — 自己申告レビューの穴を構造的に封鎖

**「作者が自分を審判しない」の再設計 (v3・評価 BL-3 への回答)**: 現行の不変条件は「実装と審判は**別プログラム**」(`config.go:70-72`、目的コメント: separate programs with separate credentials)。M2 では両役の実体が同一プログラム `hermes` になるため、この検査は**目的を保ったまま置き換える**:
1. 別プロファイル強制 — 実装役とレビュアー各々の args に含まれるプロファイル名が相互に異なること (検査は LoadConfig 時)
2. 別資格情報強制 — 実装役とレビュアーの secret_env のキー名が相互に異なること (= ゲートウェイ上の別 virtual key)
3. 構造的分離 — レビュアーは関所が起動する (実装役のプロセスや Hermes の配車がレビュー起動に関与できない) + ツリー照合
「別プログラム」検査そのものは撤廃し、上記 1-2 を新検査として実装する (Phase 0)。

### 全体の骨格

```
attendant (常駐・自前)              Hermes (純正)                        kernel (自前 CLI)
──────────────────────             ─────────────────────────────       ─────────────────────────
Backlog 監視 60s
 ├ 受付判定 (拒否既定) ─────────▶ 工程カード連鎖を生成 (--idempotency-key)
 │                                   [実装] ─▶ [レビュー A] ─▶ [レビュー B] ─▶ [検証] ─▶ [納品]
 │                                   (矢印 = Hermes の parent→child 依存。
 │                                    parent が done になると child が ready 化)
 ├ 質問投稿/回答取込 ◀──────────  needs_input ブロック/解除 (実装カード)
 └ 終端報告投稿      ◀──────────  **納品カード (連鎖の最終子) の終端状態を読む**
                                     + いずれかの工程カードの blocked (非 needs_input)
                                       を run 失敗として終端化 (下記「失敗の扱い」)

   [実装]   native worker (プロファイル lassdas-implementer) → 終了時に kernel `seal-candidate`
   [レビュー A/B] direct-command (プロファイル lassdas-review-a / -b、worker.command = kernel agent-review --reviewer <ID>)
   [検証]   direct-command (プロファイル lassdas-validate、worker.command = kernel validation verb)
   [納品]   direct-command (プロファイル lassdas-publish、worker.command = kernel controller publish)
```

- **チケットの束ね方**: 傘となる親カードは置かない (Hermes の parent は「先行依存」の意味で、束ね親は子より先に done になれず終端状態を持てないため — 評価指摘)。チケット単位の関連付けはカード title の issue key + metadata で行い、盤面のグルーピング表示に使う
- **worker.command はプロファイル単位** (カード単位ではない — `kanban_db.py:10717`)。よって工程ごとに専用プロファイルを用意し assignee で選ぶ。**Phase 1 の成果物 = プロファイル 5 個** (implementer / review-a / review-b / validate / publish) とその config.yaml 定義一式
- **契約の置き場**: 強制は封緘と納品の二点に集約。封緘が判定対象をバイト列で確定し、publish gate が「2 レビュアーの有効記録 (digest 束縛) + converged + 検証証拠」を再検算する。進行側の逸脱は「納品されない」形で表面化する (fail-safe 方向)
- **失敗の扱い (v3・評価 BL-5 への回答)**: direct-command カードは**非ゼロ終了で即 blocked・自動再試行なし** (`kanban_db.py:10725-10731` 実機仕様)。一過性障害の吸収は verb 内部の既存再試行 (レビュー最大 3 会話 `ReviewAttemptLimit` / ゲートウェイ再試行) に任せ、**verb の非ゼロ終了 = 本物の失敗**と定義する。attendant は工程カードの blocked (needs_input 以外) を検知したら run 失敗として終端報告を投稿し連鎖をアーカイブする — 人手の unblock を待つ滞留を作らない。よって「人手ゼロ」の意味は「成功時は質問回答以外の人手ゼロ / 失敗時は人手なしで正直な終端報告に到達」
- **巡別レンズと過去巡の受け渡し**: 関所がレビュー起動時に注入 ([#11](https://github.com/RHEMS-Japan/LassDas/issues/11) の実装を Phase 0 に内蔵)。差し戻し時は attendant が連鎖を 1 巡分再生成し、巡回数は連鎖 metadata で数える
- **モデル**: 実装 = anthropic/claude-opus-5、レビュー A = anthropic/claude-opus-5 (レンズ: 差分先読み)、レビュー B = openai/gpt-5.6-sol-pro (レンズ: 検証実行)。ゲートウェイ経由。実装役と同一 (baseURL,model) のレビュアーは 1 人まで — この制限は**新規実装** (現行 `models` 集合は実装役のエンドポイントを種に入れていない `config.go:559-573`)
- **ベンダー実体の突合**: 検査時点 = LoadConfig。①ベンダー名→許可ホスト対応表に対する各レビュアー baseURL の一致 ②上記「同一 (baseURL,model) は 1 人まで」。不一致 = 起動拒否 (fail-closed)。ゲートウェイの向こう側の実在は関所から検証不能 (既知の限界)
- **質問**: kernel の質問プロトコル (2 回上限・記録連鎖・期限) は無変更。attendant が実装カードの needs_input と Backlog を橋渡し (`unblock --resolve` 実装済み)。Hermes 側 block 反復上限とは質問 ID 入り理由文字列で別理由化し干渉させない
- **レビュー容量 (610)**: レビュアーはリポジトリを直接読むため詰め込み上限は構造ごと消滅。chat レビュー経路は Phase 3 で削除 ([#13](https://github.com/RHEMS-Japan/LassDas/issues/13) closed)

### seal-candidate の仕様 (v3 追記)

- **入力**: workspace パス / 読取専用ベース複製パス / チケット封筒
- **処理**: `git status --porcelain` による変更観測 → 書き込み許可範囲検査 (範囲外はエラー) → diff/ファイルビュー生成 → candidate artifact 封緘。実体は既存 `implement` verb (`cmd/worker/agent.go:99-123`) の**エージェント起動部と封緘部の分割** (「runner からの切り出し」ではない — v2 の記載を訂正)
- **AgentRun 検査の扱い**: `AgentRun.Validate` (`agent_candidate.go:44-79`) が要求する AgentID/Command 一致・PromptBytes≥1 は「関所が起動した実行」の観測値。M2 の実装役は Hermes が起動するため、candidate 側の AgentRun は**「external」種別を新設して除外**し、代わりに封緘時の観測 (変更ファイル・digest・範囲検査) だけを載せる。レビュー側の AgentRun 検査 (関所起動) は現行のまま緩めない

### 実施フェーズ

**Phase 0 — kernel の増築 (現行を壊さない・新旧は設定で切替)**
- `seal-candidate` 新設 (上記仕様)。見積もり: 実装 ~250 行 + テスト ~250 行 (分割元は既存 verb)
- `agent-review` の Hermes プロファイル起動対応: AgentSet を配列化しレビュアーごとの起動定義 (プロファイル/資格情報) を持たせる + 「別プロファイル・別資格情報」新検査 (旧・別プログラム検査の置換)。過去巡 findings のレビュー側注入 (`reviewAgentPrompt` への追加 — 現状 findings を受け取らない `cmd/worker/agent.go:254-259`)。見積もり: ~300 行 + テスト ~300 行 (v2 の ~500+400 は既存実装の進捗を過小評価していたため縮小 — 評価指摘 12)
- 実装役と同一 (baseURL,model) レビュアーの制限 + ベンダー実体突合: ~100 行 + テスト
- 非収束/機構失敗でも trail を終端報告に載せる ([#10](https://github.com/RHEMS-Japan/LassDas/issues/10))
- **テスト増強先 (実名)**: `cmd/controller` の納品ゲート判定関数群 (実測 41.1%) → 80% 以上。`internal/worker` への追加は新 verb 分のみ

**Phase 1 — Hermes 側の組み立てと attendant 改修**
- プロファイル 5 個 (implementer / review-a / review-b / validate / publish) の config.yaml 定義 + 実装役手順書 (作業規約・終了規約)
- カード生成の改修: 実体は **`internal/runtime`** (実測 30.3%・cards.go) — 「親 + 工程連鎖」型へ。**改修と同時にテスト新設し 70% 以上** (v2 の「attendant ≥70%」は対象誤り — cmd/attendant は 96 行の配線のみ)
- 失敗検知→終端化・差し戻し再生成・質問橋の attendant/runtime 実装
- **新旧切替とロールバック**: runtime config `orchestration: "runner" | "cards"`。M1 runner 経路は Phase 3 まで削除しない。**ロールバック発動 = cards 方式の進行不能 2 回連続**。**切り分けの判定 (v3)**: 工程カードに stage 成果物 (candidate/review artifact) が 1 つも残らず止まった = 設計欠陥 (ロールバック対象) / 成果物が残り revise・nonconverged 系で終わった = 実装品質 (分離条件の管轄)。判定は成果物の有無という機械条件で行い、発動時に発注者へ報告

**Phase 2 — 受入 (最終形の上で初納品)**
- 同一の実チケット (組織別モデルホワイトリスト、裁定 4 件焼き込み済み) を cards 方式で投入
- 安全性質の再実証 3 点: 台帳外実行の遮断 / 3 回目の質問が物理的に不可能 / 起票者・カテゴリ外の拒否
- 成功 = PR 作成 + 関所ログで再検算成立を確認 → 発注者の合否判断

**Phase 3 — 切除 (受入合格後のみ・1 塊ずつ PR 化し都度 build/test 全通過)**
- 処遇表の「捨てる」列: `internal/runner` 1,272 / `cmd/runner` 157 / 旧 GHA レール 2,151 / `cmd/browsercheck` 279 / `internal/state/dynamodb.go` 1,141 (ファイル単位で消せるのはここまで — DynamoStore メソッドは 7 ファイルに散在しメソッド単位の手術になるため、本フェーズでは dynamodb.go のみ即時削除し残余は [#12] 系の後続整理へ)
- chat レビュー経路 (~260 行) の削除 — 「残す」パッケージ内の関数単位削除 (処遇表では独立行にしない・二重計上防止)
- **インフラ成果物 (v3 追記)**: `deploy/pod/Dockerfile` (出荷バイナリ 4→3、CLI npm 導入の撤去) と `deploy/pod/entrypoint.sh` (worker.command 差し替え・プロファイル配置) の改訂
- 削除可否の最終確認は `go list -deps` で出荷バイナリからの到達性を実測してから

### 全 32,186 行の処遇表 (v3 実測訂正)

測定: `find . -name '*.go' -not -name '*_test.go' | xargs wc -l` (2026-08-21、非テスト Go = 32,186 行)。

| パッケージ | 行数 | 処遇 | 根拠 |
|---|---|---|---|
| internal/worker | 7,403 | 残す (関所中核) | 封緘・検算・digest 束縛 |
| internal/hook | 4,582 | 残す (関所中核) | 質問プロトコル・報告 |
| cmd/controller | 1,667 | 残す (関所中核) | 納品ゲート |
| cmd/worker | 1,758 | 残す (関所 CLI 入口) | verb 群 (chat レビュー系関数 ~260 行はここと internal/worker の内数として Phase 3 で関数単位削除) |
| internal/backlog + cmd/attendant | 697+改修 | 残す (橋) | 受付・質問・報告 |
| internal/githubapi | 2,807 | 残す | PR 納品機構 |
| internal/releaseproof | 688 | 残す | digest 連鎖 |
| internal/runtime | 625+改修 | 残す | Hermes カード橋 (Phase 1 の主改修先) |
| **internal/visiblecheck** | 605 | **残す (v3 訂正)** | **納品ゲートが現用** (`cmd/controller/commands.go:376,417`) — v2 の「到達不能」は誤り |
| cmd/setup | 1,926 | 残す (製品ツール) | 導入ウィザード |
| cmd/console | 1,050 | 残す (製品ツール) | 運用画面 |
| internal/state (dynamodb.go 除く) | 3,378 | 残す (当面) | ファイル状態系は現用 (local.go は attendant 経路)。DynamoStore メソッド残余の整理は後続 |
| internal/runner | 1,272 | 捨てる | 自前進行配線 (テスト 0%) |
| **cmd/runner** | 157 | **捨てる (v3 追加)** | 現 direct-command worker 実体 — cards 方式で不要化 |
| cmd/{reporter,questioner,app,lambda,ticker,receiver} + internal/receiver | 2,151 | 捨てる | 旧 GitHub Actions レール |
| cmd/browsercheck | 279 | 捨てる | 旧レール専用 (visiblecheck とは分離) |
| internal/state/dynamodb.go | 1,141 | 捨てる | 旧レール専用ストア |

**削減合計 = 5,000 行 / 到達点 = 約 27,200 行** (表合計 32,186 と一致)。TARGET_SHAPE.md の到達値も本数値へ再訂正 (本 PR に含む)。到達点をさらに下げる余地 (state メソッド残余・setup/console の別リポジトリ化) は後続整理とし、本移行の完了条件にしない。

### 見積もり (v3)

| フェーズ | 規模 | 作業日数 (実働) |
|---|---|---|
| Phase 0 | 新規/変更 ~650 行 + テスト ~700 行 | 2-3 日 |
| Phase 1 | runtime/attendant 改修 ~700 行 + テスト新設 + プロファイル 5 個 + 手順書 | 3-5 日 |
| Phase 2 | 受入運転 + 計測 | 1-2 日/回 × 失敗回数 |
| Phase 3 | 削除 5,000 行 + YAML 2,198 + Dockerfile/entrypoint 改訂 | 2 日 |

カレンダー 3-4 週間。

## DoD (定量)

- [ ] 実チケット 1 件が cards 方式で Backlog 起票 → PR 作成まで完走 (成功時の人手 = 質問回答のみ) — 成功時 [#11] [#13] を close
- [ ] 関所ログで publish 時の再検算成立を確認 (2 レビュアー記録の digest 束縛 + converged + validation)
- [ ] 失敗時は人手なしで正直な終端報告に到達する (工程カード blocked 滞留ゼロ — 失敗注入 1 回で実測)
- [ ] 安全 3 性質の再実証 (各 1 回の実測記録)
- [ ] 失敗終端の Backlog 報告に trail 本文が含まれる — [#10] を close
- [ ] イメージから claude/codex CLI が消えている (分離条件の未発動時)
- [ ] 処遇表「捨てる」列の削除完了・`go build ./... && go test ./...` 全通過・**非テスト Go 行数 ≤ 27,500** (測定コマンドは処遇表冒頭のもの)
- [ ] カバレッジ実測: `cmd/controller` 納品ゲート判定関数群 ≥ 80% / **`internal/runtime` ≥ 70%** (改修後)
- [ ] Hermes One で工程カード連鎖 (実装→レビュー A→レビュー B→検証→納品) が盤面に見える (スクリーンショット記録)
- [ ] 納品期限 N の計測開始 (N は発注者確定後に本 DoD へ「N 件中 1 件納品」として追記)

## 中止・分離・ロールバック条件 (事前確定)

- **実装役の分離**: 実装品質起因の受入失敗 (成果物あり・revise/nonconverged 系) が連続 2 回 → 実装役のみ claude CLI に分離
- **進行方式のロールバック**: 設計欠陥起因の進行不能 (工程カードに stage 成果物が残らない停止) が 2 回連続 → config `orchestration: "runner"` へ戻し設計に差し戻す。切り分けは成果物の有無という機械条件・発動時に発注者へ報告
- **移行自体の中止**: Phase 0 で「関所所有のレビュー起動が Hermes プロファイルでは成立しない」型の行き止まりが 3 営業日以内に解けない場合 → 発注者へ報告
- Phase 3 は受入合格まで着手しない

## 運用前提

- 配車間隔 60 秒・カード max_runtime は工程別 (実装 90 分 / レビュー各 30 分 / 検証 30 分 / 納品 15 分)。超過 = timed_out → attendant が終端化
- direct-command カードの失敗は**即 blocked・自動再試行なし** (実機仕様)。一過性障害は verb 内部の再試行で吸収し、非ゼロ終了 = 本物の失敗として attendant が終端化する (滞留させない)
- 直列前提: 単一 attendant・単一 Pod・同時 1 チケット (idempotency index 非 UNIQUE の実測制約)。並列化はスコープ外
- 納品期限 N の計数は「投入チケット数」(再試行は同一件)

## 依存関係・前提

- Hermes fork pin dbb70f61 のまま。upstream 追従はスコープ外
- ゲートウェイ (Aegis) 無変更。既知課題 (RFDEV-613/614/615) は独立進行
- Backlog 受付条件 (起票者・カテゴリ) 無変更

## スコープ外

実装役の CLI 分離 (条件発動時のみ) / 第 3 レビュアー / Slack 通知 / 真の否認不能記録 / 他トラッカー / Hermes upstream 追従 / 並列処理 / internal/state メソッド残余・setup/console 別リポジトリ化などの追加減量

## 未決定事項

- 納品期限の数値 N (発注者)
- レビュー逐次実行のレイテンシ許容値 (受入で実測後)
- 旧 GHA 構成を「削除」とするか「別リポジトリ退避」とするか (Phase 3 着手時に発注者へ 1 問)

## 参照

- docs/TARGET_SHAPE.md (最終形 — 本 PR で到達行数を ~27,200 に再訂正)
- [issue #9](https://github.com/RHEMS-Japan/LassDas/issues/9) 評価とクロスチェックの全記録
- 欠陥台帳: [#10 trail](https://github.com/RHEMS-Japan/LassDas/issues/10) / [#11 レビュアー記憶](https://github.com/RHEMS-Japan/LassDas/issues/11) / [#12 設定連動](https://github.com/RHEMS-Japan/LassDas/issues/12) / [#13 レビュー容量根治](https://github.com/RHEMS-Japan/LassDas/issues/13) — #12 は Phase 0 の設定化で、#10/#11 は Phase 0 実装で、#13 は Phase 3 削除で解消
