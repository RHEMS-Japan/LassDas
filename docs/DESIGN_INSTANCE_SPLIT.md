# 設計: フレームワークとインスタンスの分離 (2026-08-10)

## 一文で

**エンジン (本 repo) から顧客のものを全部出し、顧客ごとのインスタンス repo が「設定・知識・秘密・実行履歴」を持ち、エンジンを版数固定で呼ぶ形にする。**

発端 = 発注者指摘 (2026-08-10):「フレームワークの体裁を取るなら、本 repo が第一消費者用に動いている現状がおかしい」。指摘どおり。当時はフレームワーク repo がそのまま第一消費者のインスタンスを兼業しており、消費者の設定値・知識・秘密が同居し、**消費者のチケット本文と実装内容がフレームワーク repo の実行ログ・成果物に流れ込んでいた** (顧客データの置き場所として不適切)。本設計がこれを解消した。

## 目標の形

```
LassDas (エンジン)                      <顧客>-instance repo
├ Go コード一式                          ├ .github/workflows/receive.yml (~20 行)
├ .github/workflows/m1-worker.yml        │    └ uses: LassDas の m1-worker@<版数固定>
│   (再利用 workflow・受付〜納品の全段)   ├ config/m1-consumer.json (納品先・Backlog の ID・モデル設定)
├ config/example.json (見本のみ)         ├ knowledge/ (rules + library)
├ knowledge/README.md (見本のみ)         └ GitHub secrets / vars (App 鍵・モデルキー・HMAC 等)
└ 顧客の値・知識・秘密 = ゼロ            実行履歴と成果物はこの repo に閉じる
```

- 同一 org 内の private repo 間の再利用 workflow 参照は可能 (LassDas 側 Settings → Actions → Access を org 内公開にする 1 トグル)。過去に「不可」と確認したのは**別 org** の場合
- 起動のきっかけ (cron / 手動 / 将来のリアタイ起動) はインスタンス repo 側。リアタイ化の起動用資格情報もインスタンス repo を向く

## エンジン側の変更 (LassDas)

### 1. 二重チェックアウト

m1-worker.yml の各 job は現在、自 repo 1 つを checkout して「コード」「設定」「知識」を全部そこから取る。これを分ける:

- **エンジン checkout**: `job.workflow_sha` (= 呼び出し側が uses: で固定した版) — ツール群のビルド元。`TOOL_SHA` の意味は不変 (エンジンの版)
- **インスタンス checkout**: `github.repository` @ `github.sha` (再利用 workflow では呼び出し元 repo とその commit を指す — 2026-08-09 の実測でも tool_sha=uses の固定版 / github.sha=呼び出し元、と分離して記録されている) — 設定と知識の供給元

移行中の互換: 呼び出し元が LassDas 自身なら従来と同じファイルが見えるため、**切替前のまま動き続ける** (段階移行が可能)。

### 2. pull / question-tick をエンジン workflow へ畳み込む

現在は受信側 workflow が `go run ./cmd/receiver` / `./cmd/ticker` を直接実行しており、Go コードの checkout を受信側が持つ必要がある。これを m1-worker.yml の先頭段 (operation: ticket の前段と、tick 専用 operation) に移す。受信側 workflow は **uses: 1 行 + secrets 渡しだけの薄い皮**になる。

- 受付・報告の身分証明 (workflow_ref / repository_id) は**最上位の呼び出し元 workflow** に紐づくため、畳み込んでもインスタンス repo の身分で Lambda に到達する (再利用 workflow 内でも `github.workflow_ref` は呼び出し元を指す)

### 3. コンパイル時定数に埋まった顧客値の除去

- `cmd/controller/contract.go` の `fixedConfigSHA256` (顧客設定のダイジェストをエンジンのバイナリに焼く現行方式) を廃止。設定の信頼根拠は「インスタンス repo の保護ブランチから checkout した」という出所と、受付時に封印されるチケットの ConfigSHA256 連鎖に一本化 (各段の再検証は既存のまま)
- `internal/releaseproof` の正準ダイジェスト検査は**テスト専用設定**で固定し、顧客設定の変更でエンジンのテストが落ちる結合を切る
- `internal/m1contract` の納品先定数 (console repo 名の焼き込み) — **本設計の範囲外** (全通し納品経路のみが使用。PR 止まり納品は非依存)。設定由来化は別チケットに切り出す

### 4. 作業ツリーの顧客値ゼロ化

`config/m1-consumer.json` → 実物はインスタンスへ移し、見本 (`config/example.json`) を置く。`knowledge/` → 同様に見本化。履歴には残る (OSS は従来方針どおり新 repo 切り出しで対応・変更なし)。旧引き継ぎ資料の消費者文脈もインスタンス repo の docs へ移す。

## インスタンス側の新設

1. repo 新設 (private)。**名前の判断点は下記**
2. 受信 workflow (~20 行) + config + knowledge + docs を配置
3. GitHub secrets / vars を同名で設定 (`TICKET_INGRESS_ENABLED=false` で開始)
4. Lambda の身分検証 env を差し替え: `PULL_REPOSITORY_ID` / `PULL_REPOSITORY_SHA256` / `PULL_WORKFLOW_REF_SHA256` をインスタンス repo の値へ
5. 切替: インスタンス側 ENABLED=true・LassDas 側 false (戻しは逆順の env/vars 反転のみ = 可逆)
6. E2E: 実チケット 1 枚を新レールで PR まで流して受入

DynamoDB・トラッカー webhook URL・モデル gateway・納品先 repo・納品用 App は**無変更** (行キーに repo 身分は含まれず、過去の終端行は不活性のまま共存)。

## 順序 (各段が単独で安全)

1. エンジン変更 (§1〜3) を LassDas に実装 — 現行呼び出しのまま動作維持を実測
2. インスタンス repo 新設と配置 (無効のまま)
3. Lambda env 差し替え + 有効/無効の反転 (= 切替点。可逆)
4. 新レールで E2E 1 枚
5. LassDas 作業ツリーの顧客値ゼロ化 (§4)

規模: エンジン変更 1 日 + 切替と E2E 半日。

## 命名の決定 (発注者指示 2026-08-10)

インスタンス repo 名にフレームワーク名は**使わない** (エンジンとインスタンスの命名を独立させ、顧客側の表面には中立名だけを出す)。第 1 号インスタンスも中立名に揃えた。

## 発注者の判断点 (1 つ)

- **`m1contract` の設定由来化を本設計に含めるか**。含めると +1〜2 日。PR 止まり納品 (現運用) には影響しないため、**別チケット切り出しを推奨**

## この設計で変わらない約束

- 封印連鎖 (ticket→source→candidate→review→decision→validation→PR) の検証はすべて既存のまま
- 顧客値がフレームワークのコード・バイナリ・ログに現れない状態が「作業ツリーとして」成立 (履歴の浄化は OSS 切り出しの責務のまま)

## 実装の現在地 (2026-08-10 更新)

**Phase A 完了 (branch `feat/engine-instance-split`, commit `03b0d99`, 全 14 パッケージ green)**:
- 固定契約パッケージ (`internal/m1contract`) を削除。納品先契約は `ConsumerGitHubContract` (設定) へ移行し、参照 30 箇所を設定由来に置換
- コンパイル時 config digest (`fixedConfigSHA256`) を廃止。整合性は「封印チケットの ConfigSHA256 と読込設定の正準ダイジェストの毎実行照合」に一本化 (差し替えは実行が弾くことを実測形テストで固定)
- releaseproof / visiblecheck のテストが顧客設定ファイルを読む結合を切断 (fixture 設定へ)。StagingProof に repository を封印 (正準ダイジェスト更新済)
- 非テストのエンジンコードに顧客名ゼロ (grep 実測)

**Phase B 完了 (2026-08-10)**: 二重チェックアウト実装 (tools=エンジン固定版 / 設定・知識=呼び出し元)。pull/tick の畳み込みは**意識的に見送り** (稼働レール 14 job の条件書き換えはリスク過大) — 代替として `templates/receive-backlog-ticket.yml` (受信 workflow の雛形・顧客値は vars 参照・エンジンは deploy key で checkout) をエンジンに同梱。

**Phase C 進行中 (2026-08-10)**:
- 第 1 号インスタンス repo 新設済み: 雛形からの受信 workflow (エンジンを版数固定で pin)・config・knowledge・vars 9 件・secrets 3 件 (HMAC / Backlog / ENGINE_CHECKOUT_SSH_KEY=エンジン読み取り専用 deploy key。エンジン公開後は不要)
- **越境プローブ実測済**: model-preflight 空撃ちで contract+tools 緑 (deploy key 経由のエンジン checkout・固定版ビルド・呼び出し元からの設定供給が成立)。最終段はモデルキー未投入で失敗 = 予定どおり
- 判明済みの制約 (再調査するな): 呼び出し元 token では私有エンジン repo を checkout **できない** (実測)。deploy key 方式で解決済み

**完了 (2026-08-10 切替済み)**:
1. ~~secrets 4 件~~ → 全 7 件投入済み・instance の model-preflight 全緑 (contract / tools / model の実呼び出し)
2. ~~Lambda 身分 env 差し替え~~ → 3 値差し替え実測済み (旧値が旧 repo 身分の sha256 と 1:1 で再導出できることを先に裏取り)。ENABLED 反転済み (instance true / 旧側 false)
4. ~~作業ツリー purge~~ → config は**同名ファイルのまま見本値化** (ファイル名はエンジン/インスタンス間のパス契約のため `example.json` への改名はしない・当初案から変更)。knowledge 見本化・受信 workflow 撤去・全テスト fixture 中立化 (14 パッケージ緑)。**門番 = `internal/enginepurity`** (git 管理下の全ファイルを走査、顧客識別子 7 種を大小無視で検出。故意汚染で file:line 検出を実証済み)。CI (`ci.yml`) が push/PR ごとに執行

**完了 (2026-08-10 全工程終了)**:
3. ~~新レール E2E~~ → 実チケット 2 枚で確認。1 枚目は「依頼内容が実装済みであることを実測して無変更終了」(正直な棄却・終端報告まで配達)。2 枚目は納品先 PR 到達 (封印連鎖・証跡・AC 別の終端報告まで全段)。あわせて「書ける範囲の変更はインスタンス config の 1 commit で済む」ことも実証 (UI ディレクトリの追加)
5. ~~main へマージ~~ → PR #14 マージ済み (main `368d617`)。インスタンスのエンジン固定版も main のマージ SHA へ更新し、preflight 全緑を再実測済み

**この設計は完了した。** エンジンの作業ツリーに顧客値ゼロ (CI が門番)・インスタンスが設定/知識/秘密/実行履歴を所有・エンジンは版数固定で呼ばれる。

**運用知見 (この切替で実測)**:
- **1 プロジェクト 1 枚直列**: 封筒作成はプロジェクト単位の pending 枠 (`attribute_not_exists`) を取る。枠の返却は**終端報告完了時**。走行中に届いた次のチケットの webhook は `retry_requested` で弾かれ、トラッカー側の再送は当てにできない — 復旧はトラッカー API から当該 activity を取得して受け口 (`/backlog`, Basic 認証) へ再 POST する再演 (封筒は決定的なので delivery_id が一致する)。手順の実物はインスタンス repo の docs へ
