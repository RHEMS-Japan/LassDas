# M2 移行設計: 進行と実行の Hermes 純正化 (v2 draft 2026-08-21)

対象: docs/TARGET_SHAPE.md (確定済みの最終形) への移行。本書はその実施設計 — 何を・どの順で・どこまでやったら完了か。

## 変更履歴

- v2 (2026-08-21): 独立評価 (Gate 1 FAIL・58 点) のブロッカー 3 件と改善 11 件を反映。①レビュー実行の所有を関所に残す設計へ変更 (自己申告化の穴 T2 の封鎖) ②純正 review lane の使用を撤回し依存連鎖カード方式へ (lane の implementer 上書き問題 B2) ③全 32,186 行の処遇表を新設し TARGET_SHAPE の到達行数を実測で訂正 (B3) ④テスト増強先を納品ゲート/attendant へ変更 ⑤ロールバック条件を新設
- v1 (2026-08-21): 初版

## 概要

現行 (M1) は「受付・進行・実行・関所」を全て自前 Go が駆動し、実行だけ claude/codex CLI を外部呼び出しする。M2 では**進行を Hermes 純正カンバンに、実装の実行を Hermes 純正エージェントに移し、自前 Go は関所 (policy kernel) に縮める**。ハーネスは 1 本になり、CLI 同梱・版追従・自前進行配線 (テスト 0% の 1,272 行) が消える。

## 背景と根拠 (要約)

- 維持/廃止評価 + 独立 2 系統クロスチェックの帰結 ([issue #9](https://github.com/RHEMS-Japan/LassDas/issues/9)): 実失敗 3 件 (606/608/610) は全て自前進行配線で発生。価値は関所に集中
- 発注者決定 (2026-08-21): 道 2 直行 (現行形での受入再挑戦なし) / 実行も Hermes 純正 (一番シンプルな形から始め、不足したら分離)
- 608 型の非収束対策 (巡別レンズ・過去巡の受け渡し) は、v2 ではレンズ注入を関所が担う (下記「レビューの所有」)

## 設計

### 信頼境界 (v2 の核心 — 評価指摘 T2 への回答)

役割を「作る側」と「判定する側」で分け、**判定する側の実行は関所が所有し続ける**:

- **実装役 = 作る側 = 非信頼**。Hermes 純正エージェントが自由に作業する。成果物は関所が封緘 (後述の新 verb) してバイト列と digest で確定する。実装役の申告は一切信用しない — どうせレビューと検証が判定するため、ここに証明は不要
- **レビュー役・検証 = 判定する側 = 関所の所有物**。レビューは関所自身が子プロセスとして起動する (`worker agent-review` の系譜)。起動したのが関所である以上、「どの接続先のどのモデルが・どの digest の成果物を読んだか」は自己申告ではなく**関所自身の観測**になる。レビュー後の作業ツリー照合 (レビュアーが手を出していないことのバイト照合) も現行のまま生きる
- この分割により「Hermes 実行 = 自己申告レビューを関所が鵜呑みにする」穴 (評価 T2: DecideStage の一意性検査は ReviewerID/RequestID のみで、1 エージェントが 2 レビューを偽装可能) は**構造的に発生しない**。ingest 型 (外部レビューの持ち込み) の verb は作らない

ハーネス 1 本の原則は維持される: 関所が起動するレビュアーの実体も **Hermes 純正エージェント** (`hermes -p <レビュアープロファイル> --cli chat -q <レンズ付きプロンプト>`) であり、claude/codex CLI は使わない。「1 本のハーネスを、実装は Hermes の配車が、レビューは関所が、それぞれ起動する」が M2 の形。

### 全体の骨格

```
attendant (常駐・自前)              Hermes (純正)                        kernel (自前 CLI)
──────────────────────             ─────────────────────────────       ─────────────────────────
Backlog 監視 60s
 ├ 受付判定 (拒否既定) ─────────▶ 親カード + 工程子カード生成
 │                                  (依存連鎖・--idempotency-key)
 ├ 質問投稿/回答取込 ◀──────────  needs_input ブロック/解除
 └ 終端報告投稿      ◀──────────  親カード done/blocked
                                    [実装カード] 純正エージェント ───▶ 新 verb: seal-candidate
                                        (native worker・自由作業)         (workspace→diff/digest 封緘)
                                    [レビューカード] 直接コマンド ───▶ 既存系 verb: agent-review
                                        worker.command = kernel            (関所が Hermes プロファイル
                                                                            reviewer-A / reviewer-B を
                                                                            順に起動・記録・ツリー照合)
                                    [検証カード]   直接コマンド ─────▶ validation evidence (既存)
                                    [納品カード]   直接コマンド ─────▶ controller publish
                                                                          (関所: 全再検算 → PR 作成)
```

- **契約の置き場**: Hermes 側の進行は緩くてよい。**強制は封緘と納品の二点に集約** — 封緘 (seal-candidate) が「何を判定対象とするか」をバイト列で確定し、publish gate が「設定された 2 レビュアーの有効な記録 (digest 束縛) + converged 決定 + 検証証拠」を再検算する。進行側の逸脱は「納品されない」という形で必ず表面化する (fail-safe 方向)。台帳外カードが 1 行で死ぬ現行原理の一般化
- **カード方式 (v2 決定)**: 純正 review lane は**使わない**。根拠 (評価 B2 で確定): lane は実装役 1 枠・レビュアー 1 枠の設計で、レビュアー A が B へ回すと implementer が A に上書きされ (`kanban_db.py:6549`)、B の差し戻しが本来の実装役に届かない。代わりに**依存連鎖の子カード** (実装 → レビュー A → レビュー B → 検証 → 納品) を使う。レビュー/検証/納品カードは direct-command worker (worker.command = kernel verb) — direct-command が skills/model 上書きを無視する制約は、これらのカードがモデル選択を関所の設定から行うため影響しない
- **巡別レンズと過去巡の受け渡し (608 対策の担い手変更)**: lane を使わないため Hermes 純正の巡回数計数は使えない。代わりに**関所がレビュー起動時にレンズと過去巡 findings を注入する** — これは既存の欠陥台帳 [#11](https://github.com/RHEMS-Japan/LassDas/issues/11) の実装そのものであり、M2 Phase 0 に含める (差し戻し時は依存連鎖を 1 巡分再生成し、stage 番号を親カードの metadata で数える)
- **モデル**: 実装 = anthropic/claude-opus-5、レビュー A = anthropic/claude-opus-5 (レンズ: 差分先読み)、レビュー B = openai/gpt-5.6-sol-pro (レンズ: 検証実行)。いずれもゲートウェイ経由。実装役と同一 (baseURL,model) のレビュアーは 1 人まで (2 人目のベンダー分離は既存検証が強制)
- **ベンダー実体の突合 (評価 T9 への回答)**: 検査時点 = 設定読み込み時 (LoadConfig)。検査内容 = ①ベンダー名→許可ホストの対応表 (config 内で宣言) に対し、各レビュアーの baseURL ホストが一致すること ②実装役と同一 (baseURL,model) の組をレビュアー 2 人が同時に持たないこと。不一致 = 起動拒否 (fail-closed・モデル呼び出し 0 回で停止)。対応表自体はゲートウェイ 1 段のため実質 1 行 (gateway ホスト) — この検査が守るのは「設定ミスと名前の偽装で 2 ベンダー要件が無音で骨抜きになること」であり、ゲートウェイの向こう側の実在は関所からは検証不能 (既知の限界として記録)
- **質問**: kernel の質問プロトコル (2 回上限・記録連鎖・期限) は無変更。attendant が needs_input カードと Backlog 質問/回答を橋渡し (回答採用 → `unblock --resolve` は実装済み機構を流用)。**二重計数の整合 (評価指摘)**: Hermes 側の block 反復上限は同一理由の再ブロックを triage 送りにする別系統の安全弁 — 質問は 1 回ごとに理由文字列へ質問 ID を含めて別理由化し、干渉させない
- **レビュー容量問題 (610)**: レビュアーは Hermes エージェントとしてリポジトリを直接読むため、詰め込み上限は構造ごと消滅。chat レビュー経路は Phase 3 で削除 ([#13](https://github.com/RHEMS-Japan/LassDas/issues/13) closed)

### 実施フェーズ

**Phase 0 — kernel の増築 (現行を壊さない・新旧は設定で切替)**
- **新 verb `seal-candidate`**: 実装カードの workspace を読取専用ベース複製と突合し、diff/digest/candidate artifact を封緘する (現行は runner 内部処理 — それを外部起動可能な verb に切り出す)。見積もり: 実装 ~400 行 + テスト ~300 行 (切り出し元 `internal/worker` の封緘系は 77.7% カバレッジの既存実装を流用)
- **`agent-review` の一般化**: 単一 Reviewer 型 (config.go:51-73) を「レビュアー配列 + 指名起動」に拡張し、起動コマンドを設定化 (Hermes プロファイル指定)。レンズ + 過去巡 findings + 巡回数の注入を実装 (#11)。見積もり: ~500 行 + テスト ~400 行
- ベンダー実体突合 (上記) + 実装役と同一 (baseURL,model) レビュアーの制限: ~100 行 + テスト
- 非収束/機構失敗でも経過記録 (trail) を終端報告に載せる ([#10](https://github.com/RHEMS-Japan/LassDas/issues/10))
- **テスト増強先 (評価 T6 への回答)**: 納品ゲート (cmd/controller、実測 41.1%) と publish 再検算経路を最優先で厚くする — 全強制を集約する一点が最薄なのは許容しない。目標: controller の gate 判定関数群を 80% 以上。internal/worker への追加は新 verb 分のみ

**Phase 1 — Hermes 側の組み立てと attendant 改修**
- 実装カード用プロファイル + 手順書 (実装役の作業規約・seal-candidate 呼び出し)。レビュー/検証/納品カードの worker.command 定義
- attendant のカード作成を「親 + 依存連鎖子カード」型に変更 (~600 行改修、現行 attendant は実測カバレッジ 0% → **改修と同時にテストを新設し 70% 以上にする**)
- **新旧切替とロールバック (評価 T7 への回答)**: 進行方式は runtime config の 1 キー (`orchestration: "runner" | "cards"`) で選択。M1 の runner 経路は Phase 3 まで削除しない。**ロールバック = config を "runner" に戻して Pod 再起動** (受入で cards 側が設計欠陥により 2 回連続で進行不能になった場合に発動 — 実装品質起因の失敗はロールバック対象外で分離条件の管轄)
- 質問橋の接続 (needs_input ⇄ 既存質問プロトコル)

**Phase 2 — 受入 (最終形の上で初納品)**
- 同一の実チケット (組織別モデルホワイトリスト、裁定 4 件焼き込み済み) を cards 方式で投入
- 安全性質の再実証 3 点: 台帳外カード遮断 / 3 回目の質問が物理的に不可能 / 起票者・カテゴリ外の拒否
- 成功 = PR 作成 + 関所ログで再検算の成立を確認 → 発注者の合否判断

**Phase 3 — 切除 (受入合格後のみ・1 塊ずつ PR 化し都度 build/test 全通過)**
処遇表 (下記) の「捨てる」列を実施。削除可否の最終確認は `go list -deps` で出荷 4 バイナリからの到達性を実測してから (評価 T8 への回答 — 特に internal/state は cmd/setup・internal/runtime から参照があるため、DynamoDB 系のファイル単位で分割削除する)。

### 全 32,186 行の処遇表 (評価 B3 への回答・実測値)

| パッケージ | 行数 | 処遇 | 根拠 |
|---|---|---|---|
| internal/worker | 7,403 | **残す** (関所中核) | 封緘・検算・digest 束縛 |
| internal/hook | 4,582 | **残す** (関所中核) | 質問プロトコル・報告 |
| cmd/controller | 1,667 | **残す** (関所中核) | 納品ゲート |
| cmd/worker | 1,758 | **残す** (関所 CLI 入口) | verb 群 |
| internal/backlog + cmd/attendant | 697+改修 | **残す** (橋) | 受付・質問・報告 |
| internal/githubapi | 2,807 | **残す** | PR 納品機構 (third-party 依存 0) |
| internal/releaseproof | 688 | **残す** | digest 連鎖 |
| internal/runtime | 625 | **残す** | Hermes カード橋 |
| cmd/setup | 1,926 | **残す** (製品ツール) | 導入ウィザード — 移行の対象外 |
| cmd/console | 1,050 | **残す** (製品ツール) | 運用画面 — 移行の対象外 |
| internal/state | 4,519 | **分割** | ファイル状態系は残す / DynamoDB 系 (dynamodb.go 1,141 + DynamoStore メソッド群) は旧レール専用 → 捨てる。境界は go list -deps 実測で確定 |
| internal/runner | 1,272 | **捨てる** | 自前進行配線 (テスト 0%) |
| chat レビュー経路 | ~260 | **捨てる** | #13 |
| cmd/{reporter,questioner,app,lambda,ticker,receiver} + internal/receiver | 2,151 | **捨てる** | 旧 GitHub Actions レール |
| cmd/browsercheck + internal/visiblecheck | 884 | **捨てる** | 到達不能 (旧レール専用) |
| .github/workflows/m1-worker.yml | (YAML 2,198) | **捨てる** | 旧レール |

**到達点の実測: 約 32,200 − 約 8,000〜9,000 = 約 23,000〜24,000 行** (うち関所中核 ~15,600 + PR/証跡/橋 ~4,100 + 製品ツール ~3,000)。TARGET_SHAPE.md v1 の「約 1 万数千行」は誤り — 同文書を本表の実測値に訂正する (本 PR に含める)。

### 見積もり (v2 実数)

| フェーズ | 規模 | 作業日数 (実働) |
|---|---|---|
| Phase 0 | 新規/変更 ~1,000 行 + テスト ~900 行 | 3-4 日 |
| Phase 1 | attendant 改修 ~600 行 + テスト新設 + プロファイル/手順書 | 3-5 日 |
| Phase 2 | 受入運転 + 計測 | 1-2 日/回 × 失敗回数 |
| Phase 3 | 削除 ~8,000 行 + YAML + 分割実測 | 2-3 日 |

カレンダー 3-4 週間 (v1 比 +1 週間 — Phase 0 が「検証の追加のみ」ではなく verb 増築であることが評価で確定したため)。

## DoD (定量)

- [ ] 実チケット 1 件が cards 方式で Backlog 起票 → PR 作成まで完走 (人手ゼロ、質問回答を除く) — 成功時 [#11] [#13] を close
- [ ] 関所ログで publish 時の再検算成立を確認 (2 レビュアー記録の digest 束縛 + converged + validation)
- [ ] 安全 3 性質の再実証 (各 1 回の実測記録)
- [ ] 失敗終端の Backlog 報告に経過記録 (trail) 本文が含まれる — [#10] を close
- [ ] イメージから claude/codex CLI が消えている (分離条件の未発動時)
- [ ] 処遇表「捨てる」列の削除完了・`go build ./... && go test ./...` 全通過・非テスト Go 行数 ≤ 24,500 (実測条件)
- [ ] cmd/controller の納品ゲート判定関数群カバレッジ ≥ 80% / 改修後 attendant ≥ 70% (実測条件)
- [ ] Hermes One で工程カードの流れ (実装→レビュー A→レビュー B→検証→納品) が盤面に見える (スクリーンショット記録)

## 中止・分離・ロールバック条件 (事前確定)

- **実装役の分離**: 実装品質が原因の受入失敗が連続 2 回 → 実装役のみ claude CLI に分離 (TARGET_SHAPE の規定)
- **進行方式のロールバック**: cards 方式が設計欠陥により進行不能 (カードが流れない・橋が壊れる類) を 2 回連続 → config で runner 方式へ戻し原因を設計に差し戻す
- **移行自体の中止**: Phase 0 の新 verb 設計で「関所所有のレビュー起動が Hermes プロファイルでは成立しない」型の行き止まりが 3 営業日以内に解けない場合 → 発注者へ報告
- Phase 3 は受入合格まで着手しない

## 運用前提 (評価 T11 への回答)

- 配車間隔 60 秒 (dispatch_interval_seconds)・カード max_runtime は工程別に設定 (実装 90 分 / レビュー各 30 分 / 検証 30 分) — 超過は timed_out 終端として trail に残す
- 直列前提: 単一 attendant・単一 Pod・同時 1 チケット (idempotency index 非 UNIQUE の実測制約による)。並列化はスコープ外
- レート制限・ゲートウェイ障害時はカード失敗 → Hermes の失敗上限 (failure_limit) が再試行を抑止 → 終端報告。納品期限 N の計数は「投入したチケット数」で数え、再試行は同一件

## 依存関係・前提

- Hermes fork は現行 pin (dbb70f61) のまま。upstream 追従はスコープ外
- ゲートウェイ (Aegis) は無変更。既知課題 (RFDEV-613/614/615) は独立進行
- Backlog 側の受付条件 (起票者・カテゴリ) は無変更

## スコープ外

実装役の CLI 分離 (条件発動時のみ) / 第 3 レビュアー追加 / Slack 通知 / 真の否認不能記録 (非対称署名) / 他トラッカー / Hermes upstream 追従 / 並列処理

## 未決定事項

- 納品期限の数値 N (発注者)
- レビュー逐次実行のレイテンシ許容値 (受入で実測後)
- 旧 GHA 構成を「削除」とするか「別リポジトリ退避」とするか (Phase 3 着手時に発注者へ 1 問)

## 参照

- docs/TARGET_SHAPE.md (最終形 — 本 PR で行数を実測値に訂正)
- [issue #9](https://github.com/RHEMS-Japan/LassDas/issues/9) 評価とクロスチェックの全記録
- 欠陥台帳: [#10 trail](https://github.com/RHEMS-Japan/LassDas/issues/10) / [#11 レビュアー記憶](https://github.com/RHEMS-Japan/LassDas/issues/11) / [#12 設定連動](https://github.com/RHEMS-Japan/LassDas/issues/12) / [#13 レビュー容量根治](https://github.com/RHEMS-Japan/LassDas/issues/13) — #12 は Phase 0 の設定化で、#10/#11 は Phase 0 実装で、#13 は Phase 3 削除で解消
