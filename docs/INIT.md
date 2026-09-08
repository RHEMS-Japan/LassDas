<!-- evaluate: 92/100 | independent-second: 93/100 | blockers: 0 | stage: review | type: feature | date: 2026-09-08 -->
<!-- provenance: v0.2 independent reviews 87/100 (0 blockers), 85/100 (1 blocker); one correction/re-evaluation round, final 92/100 and 93/100 (both 0 blockers). Design approval does not certify runtime acceptance. -->
# lassdas init — 一本で立ち上げが終わる入口 (設計書 v0.2)

発注者決定 (2026-09-08): **setup は init に改名し、init だけで立ち上げを終わらせる。終わりは「依頼を 1 本流して、納品先に Pull Request が出ること」。** 最初の対象はシンプルな CLI アプリ。本体は手元の Docker で Pod と同じイメージを動かす。入口は既存トラッカー、役の道具は Hermes。モデルは OpenAI 互換の URL と役ごとの鍵を入力する。

**トラッカー・GitHub・モデルの鍵は、利用者が外で取得して入力する。** init が取得先と必要な権限を案内し、入力後に検査する。資格情報の発行や権限変更の自動化は不要。

本書は設計であり、以下の init、run、`kind: cli` は未実装。現行コードの事実、今回の実測、実装後の受入を分ける。用語は [TRUST_MODEL.md](TRUST_MODEL.md)、本体は [RUNTIME_POD.md](RUNTIME_POD.md)、設計工程は [INVESTIGATING_DESIGNER.md](INVESTIGATING_DESIGNER.md) に従う。

変更履歴: v0.1 (2026-09-08) 初版。独立評価 2 系統は 47/100 と 61/100、重複を除くブロッカー 3 件。→ v0.2 (同日) 外部取得した鍵の入力、本人の鍵による動作確認、イメージ内検証、秘密保護、生成設定と鍵の対応、CLI の契約と編集範囲、実測と受入の区別を反映。本版の独立評価後に板の名前・検証入口・鍵の対応を補い、1 回の再評価で 92/100 と 93/100、両方ブロッカー 0。設計を review 段階へ進めた。

## 0. この入口が要る根拠

現行 setup.sh は `go run ./cmd/setup` を起動する。`cmd/setup/main.go` の段は GitHub 構築・クラウド構築・トラッカー構築・model-preflight の 4 つ。終了時にも toolchain と検証コマンドの手編集を求める。前提は gh・go・git。これでは現在の本体の設定と最初の PR が揃わない。

本体は [Dockerfile](../deploy/pod/Dockerfile) と [entrypoint](../deploy/pod/entrypoint.sh) に必要な部品を既に持つ。init はその入力と順序を揃えて確かめる。

| 数える対象 | 現行コードの内訳 |
|---|---|
| Go バイナリ | attendant、runner、worker、controller、browsercheck、statusboard、agentexec、計 7 |
| tool-pins.txt | worker、controller、browsercheck の SHA-256、3 本分 |
| boot が書く profile | runner 1、validate/publish/investigate/design-decide 4、e2e 1、配送 3、実装 1、レビュー 2、設計レビュー 2、写し 1、計 15 |
| 本設計のカード用 profile | 実装系 5 + 設計系 5、計 10。runner は別枠。観測・配送用は使用しない |
| モデルの鍵 | §7 の 8 身元。設計レビューを別に指定する場合は 10 本 |

鍵を共有する役は予算監視でもまとめられる (`internal/attendant/budget.go` の roleProbes)。別の役へ同じ鍵を設定しないことと、費用内訳が正しく分かれることは別の検査であり、後者は §10 で測る。私的な個別の運用記録を公開設計の実測根拠にしない。

## 1. 一枚の絵

```text
lassdas init
 ├ 1 準備      GitHub の鍵 → repo と枝を読む / Docker と固定イメージを確認
 ├ 2 納品先    編集範囲・検証手順を提案 → 確認 → イメージ内で試走
 ├ 3 入口      トラッカーの接続先・bot の鍵・許可起票者 → カテゴリと列を確認
 ├ 4 モデル    URL・モデル・役ごとの鍵 → 設定検査と疎通
 ├ 5 本体      設定・env・volume → 起動時検査 → 認証つきの板を確認
 └ 6 動作確認  本人の鍵 (保存しない) → 依頼の内容を確認 → 起票
               受付 → 調査・設計 → 設計レビュー A/B → 写し役
               → 候補レビュー A/B → 検証 → PR の実在・差分を確認 → 完了

lassdas run start / stop / status / logs
```

本体は `orchestration: cards` を明示する。現行の省略時・見本の runner 方式とは異なり、既存の設計工程を備える cards を選ぶ。別の進行機構は作らない。init が内部で run start 相当を呼ぶため、立ち上げに別コマンドの実行を要求しない。

### 1.1 init 自体の入手と前提

初版は既存のソース配布を使う。フレームワークのソースで `go build -o lassdas ./cmd/lassdas`、生成した実行ファイルを納品先 repo 内から起動する形を B で用意する。cmd/lassdas は init と run の小さな入口、対話と再開処理には cmd/setup の骨格を使う。setup.sh は新しい起動方法を案内する互換入口とし、旧状態ファイルを新形式として読み込まない。

**ソース版の前提は git・Go (go.mod の版)・Docker Desktop と取得済みの鍵。** gh のログインを鍵入力の代わりにはしない。v0.1 の「Docker だけの機械」は現行の入手方法と両立しないため受入から外す。単体バイナリの配布基盤や新しいレジストリは本書で作らない。

イメージは既存の配布者から受け取る参照・digest・対応するソースの 40 桁 SHA を入力するか、配布情報から読む。非公開なら取得用認証も外で済ませる。init は digest で取得し、tool-pins.txt と実バイナリを照合する。現行 Dockerfile の ENGINE_SHA は ARG の検査だけで永続化されず、イメージからソース SHA を取得できるとは扱わない。既存のビルド記録で対応を確認できない場合は準備未完了。ローカル起動のために既存の本番 release script を実行しない。

## 2. 保存するものと再開

| 場所 | 内容と所有者 |
|---|---|
| `~/.lassdas/<project>/` (0700) | init の台帳、設定 2 本、runtime.env (0600)。利用者所有、納品先 repo の外 |
| named volume → /data | ledger.db、kanban.db、runs、knowledge、秘密ファイル。関所 uid 1000 所有。通常の停止で消さない |
| 設定 → /etc/lassdas/config (read-only) | runtime.json と consumer.json。/etc/lassdas 全体を覆うと tool-pins.txt が隠れるので禁止 |
| init の台帳 | 非秘密の回答、段の入力の指紋、生成物 ID、検査結果、動作確認の相関 ID・issue ID・PR URL・所要時間。鍵の値やハッシュは含めない |

常用の鍵は env ファイルに保存し、entrypoint が納品用トークンと板の鍵を /data/secrets/ の 0600 ファイルへ移す。管理者の鍵と動作確認の本人の鍵はメモリ限り。ログ・環境変数・コンテナ・設定へ渡さない。

台帳の「済み」は現在の認証の証明ではない。再開時は必要な鍵の疎通、repo と枝、image digest、設定、コンテナの実在を再検査する。失効はその段で止め、入力し直した常用鍵を原子的に保存する。`--redo <段名>` は依存する後段を未完了に戻す。稼働中の設定変更は対象コンテナを停止してから差し替え、他 project の状態を触らない。

## 3. AC1 — 各段で聞くことと確かめること

| 段 | 入力 | 検査・生成・再開 |
|---|---|---|
| 1 準備 | GitHub トークン、repo (remote があれば候補)、image 参照と対応ソース SHA | 認証後に private repo も読む。repo の数値 ID、枝の実在、Docker の接続・arm64、digest と pin を確認。取り込み枝は repo の規則を読み利用者が確認し、default branch と同じだと決め打たない |
| 2 納品先 | cli 固定。範囲、上限、toolchain、install_command、verify_commands (1〜4)、verify_working_directory。Go/Node の構成から提案 | 同じ digest の使い捨てコンテナで、確定した枝 SHA の clone に対して版を測り、install と検証を実行。モデル/トラッカー鍵は入れず、clone 後は GitHub 鍵も除く。納品時と同じ worker の検証関数を B の入口から使う。ホストで通った結果で代用しない |
| 3 入口 | トラッカー接続先、project、許可起票者 ID、bot の鍵。必要時だけ管理者の鍵 | project・利用者・bot の本人情報・カテゴリ・4 状態の対応を取得。既存列を再利用し、不足分の一覧を示して確認後に作成。現行 setup は追加列 2 本と既存状態を使うので「4 本新設」ではない。作成済み ID を再利用 |
| 4 モデル | URL、モデルと鍵 (§7)。会社名は認識できる接頭辞から提案、不明なら聞く | 本体と同じ設定検査と全身元の疎通。別身元の同じ鍵は拒否。consumer と env を同じ回答から生成。再開時も鍵を検査。API 呼出しの費用は実行前に案内 |
| 5 本体 | 設定・保存先・板の接続先をまとめて確認 | §5 の設定読込、必須秘密ファイルの実在、別ユーザ起動、読取拒否、常駐の生存、板の認証成功と未認証拒否。ログの一語だけで成功判定しない。既存コンテナのラベル・設定指紋を照合し重複起動しない |
| 6 動作確認 | 本人が外で取得した鍵、小変更の提案、作る依頼・枝・PR の確認 | 本人 API の ID と allowed_creator_id の一致。起票に必要な種別・優先度は実在一覧から解決。PR の差分と確定記録を確認 (§8)。本人の鍵は起票終了・中断で破棄、redo も再入力 |

GitHub の Contents/Pull requests の write は必要権限として案内する。repo の read 成功だけでは write を確認済みにしない。実際の枝と PR の作成成功で確定し、不足時は失敗した操作と必要権限を示す。設定や権限を変更して補わない。

段 2 の image 内の入口は、B で既存 worker に加える薄い `check-consumer` サブコマンドとする。consumer の設定・repo-root・確定した base SHA を受け、既存の consumer 検査と検証関数を共有し、測定した toolchain・コマンドと終了結果を返す。モデル未設定のこの段で全体の models/agents は要求しない。現在の `run-validation` は ticket/source/candidate を必須にするため初期試走には流用しない。`docker run --entrypoint /usr/local/bin/worker … check-consumer …` として通常の常駐 entrypoint を起動しない。初期試走の記録を候補の封緘証拠として納品へ流用することもない。

トラッカーの管理者鍵は、必要な列を作る権限が bot に無い場合に必要。作成後は破棄する。webhook は新設しない。起票者の根拠は `internal/receiver/event.go` と `internal/state/resume.go` の ID 厳密一致。bot を許可者へ一時変更せず、本人の鍵で確認する。

質問数は「値を入力・選択する欄」を 1、まとめた確認画面も 1 と数え、1 画面の多数の欄をまとめて 1 としない。7 モデル + 8 鍵 + URL だけで 16 欄、接続先・repo・起票者・検証手順・確認・必要時の会社名等が加わる。**25 欄・入力 10 分は未検証の改善目標であり、合否条件にはしない。** §10 で既定・追加・再入力を別々に測る。鍵の外部取得とダウンロード時間も分けて記録する。

## 4. AC2 — 納品先の種類 cli

### 4.1 残す契約と持たない工程

consumer に kind を追加。省略/web は従来どおり、未知値はエラー。cli の delivery は pull_request のみ。GitHub 契約を丸ごと外さず、repository 確認・baseline・PR 作成の共通契約から web 固有の項目だけを分岐する。

| cli でも必須 | cli に不要・実行しない |
|---|---|
| repo 名/数値 ID/active 状態、取り込み枝、観測した default branch の照合、base/head/tree SHA、候補差分の結び付け、編集範囲・上限・禁止語、toolchain・検証結果、PR 宛先 | release_branch、stg/本番 origin/login URL、deploy workflow と名前照合、画面観測、checks/integrate/promote、マージ設定の一致条件と変更 |

ダミーの release_branch や origin で検査を通さない。cli の観測・配送設定は黙って無視せず設定エラー。マージは人。default branch・ruleset・レビュー条件・CI は変更しない。

同じ kind を runtime.report_destinations にも持たせる。現行 `internal/hook/report_protocol.go` の ReportDestination.validate は PR 納品にも origin 2 本を要求し、consumer だけ変えても起動しない。cli は origin 不要・PR のみとする。終端の「PR URL 必須、commit/deploy 証拠禁止」は既存どおり。consumer と報告先の repo・kind・納品段の一致を起動前に照合する。

mode.validation_command は新設しない。既存の mode.toolchain (最大 4)、install_command (必須)、verify_commands (1〜4)、verify_working_directory を使う。コマンドは argv、1 本の上限は現行 10 分。Go なら go mod download と go test ./... を提案できるが、実際の repo/image で確認する。検証後の候補 bytes 不変・証拠の封緘も維持する (`internal/worker/validation_evidence.go`)。

### 4.2 編集範囲

現行 allowed_file_prefixes は末尾 / のディレクトリのみで、除外指定も root 全体の指定もできない。init は隠れた名前 (.github/ 等) を除いた top-level ディレクトリを候補にする。全体を許可したと表示しない。

root の main.go や README.md がある普通の CLI を扱うため、A で既存リストに **root のファイル名の完全一致**を加える。末尾 / は従来の prefix、末尾 / が無い root 名はその 1 ファイルだけ。空・.・絶対パス・..・隠れた名前・symlink は許さず、wildcard や除外式は増やさない。初期値は Git 管理下の実在ファイルから提案し、利用者が確認した一覧だけを採用。未指定の新しい root ファイルまで自動で許可しない。

候補列挙 (derive.go)、依頼/候補の検算 (ticket.go/artifact.go/agent_candidate.go)、役の変更検査 (agent.go)、設計の範囲 (investigate/records.go) で同じ完全一致を適用する。既存 prefix の意味を維持し、main.go.bak・隣のディレクトリ・.github/・symlink の拒否を回帰で固定する。既存の明示リストで root ファイルを表す変更に限り、新しい権限管理機構にはしない。

### 4.3 受付と影響調査

文言欄は現行 `internal/worker/ticket.go` でも全空を許す。cli は画面文言照合を要求しないが、自由文を消したり形式で拒否したりしない。CLI の標準出力の約束は依頼とテストで検証。実際の web 画面の確認が必要なら、対象・確認方法を選択肢で聞き、勝手に依頼を変えない。既存の受付応答処理・再試行は作り直さない。

probe は空でよい (validateProbes)。組み込み repo.list/read/grep は使える。影響候補は次で再現できる。検索結果は全件の編集を要求する一覧ではない。

```sh
rg -n 'StagingOrigin|ProductionOrigin|StagingWorkflow|ProductionWorkflow|StagingLoginURL|Contract\(\)' internal cmd --glob '*.go' --glob '!**/*_test.go'
rg -n 'AllowedFilePrefixes|allowedPath|withinPrefixes' internal --glob '*.go' --glob '!**/*_test.go'
```

A の主な対象は internal/worker、internal/githubapi、cmd/controller、internal/hook/report_protocol.go、internal/runtime。runner/e2e、attendant/session、visiblecheck、releaseproof は cli から呼ばれないことを確認し、到達しないなら変更しない。

## 5. AC3 — Docker と生成設定

初版の実測対象は Apple Silicon の macOS + Docker Desktop + linux/arm64。Dockerfile が arm64 以外を拒否する。Intel Mac、x86、rootless Docker、Podman、別の Docker 実装は未検証で対象外。自動で別方式へ切り替えない。

### 5.1 起動と保存

台帳と走行は named volume。新規 volume は同じ image の使い捨て準備コンテナで uid/gid 1000 所有にし、関所から書けることを試す。本体は image 既定の uid 1000。準備コンテナに鍵を渡さず、所有者変更は新規 volume の所定ディレクトリだけ。既存 volume を一括 chown しない。

Docker socket、ホスト HOME、作業 repo、本番の鍵や身元は mount しない。設定だけ read-only、作業 clone は volume 内。host の project 親は 0700、配下の非秘密 config ディレクトリは 0755、JSON は 0644 とし、その config ディレクトリだけを mount してコンテナ uid 1000 から読めることを確認する。runtime.env (0600) はその外に置く。volume 準備では `--entrypoint /bin/sh` 等を明示し、通常 boot による gateway 必須検査や常駐を起動しない。capability 不足や no-new-privileges 設定で動かないときは起動時検査に従って停止。privileged や seccomp 無効化を自動で追加しない。

statusboard はコンテナ内 :9200、LASSDAS_BOARD_USER と 16 文字以上の password が必須。init は user と 32 文字のランダム password を生成し LASSDAS_BOARD_PASS へ設定。host は `-p 127.0.0.1:9200:9200` のみ。使用中なら空き port を提示して選択を保存。hermes serve の 127.0.0.1:9119 とは別。

未認証拒否と認証成功の試験対象は板の `/`。`/healthz` は意図的に認証不要なので、ここに未認証拒否を要求しない。

板は初版では閲覧用で、回答はトラッカーから行う。本人の鍵を板へ常用保存する必要はない。板の操作用鍵を任意で設定する場合だけ既存の tracker 関連 env 3 項目を揃える。受付は attendant の 60 秒周期、dispatch も 60 秒周期。新しい webhook は不要。

### 5.2 runtime.json と env

| 項目 | 出どころ・値 |
|---|---|
| ledger_path / consumer_config_path / knowledge_root | /data/ledger.db、/etc/lassdas/config/consumer.json、/data/instance (読み書き可) |
| tracker | 段 3 の接続先と実在 ID。allowed_activity_type=1、allowed_creator_id は本人 1 人、カテゴリと 4 状態、operator_user_ids は同じ本人 |
| identity | repository/repository_id は本体ソース repo の API で確認した身元、engine_sha は §1.1 のビルド記録。workflow_ref はその repo + /local-runtime@ + engine_sha という安定名 (workflow を作る意味ではない) |
| automation_run_id | 初回に UTC 日付 + 12 ランダム bytes で run_YYYYMMDD_<24 hex> を作り、再開時も保持 |
| report_destinations | 段 1 の repo、kind=cli、delivery=pull_request。§4 の実装が前提 |
| worker_bin / controller_bin / browsercheck_bin | /usr/local/bin/worker、/usr/local/bin/controller、browsercheck は空 |
| worker_sha256 / controller_sha256 | image の /etc/lassdas/tool-pins.txt から取得し実バイナリと照合。3 本目も存在検査するが観測を有効にしない |
| hermes_bin / hermes_board / hermes_profile | /usr/local/bin/hermes、project ごとの内部名、lassdas-runner |
| orchestration / chain | cards、runs_root=/data/runs、target_token_path=/data/secrets/target-token、failure_streak_limit=3、profile 10 本を entrypoint と一致させる |
| chain.profiles の実装系 5 本 | implementer、review_a、review_b、validate、publish → lassdas-implementer、lassdas-review-a、lassdas-review-b、lassdas-validate、lassdas-publish |
| chain.profiles の設計系 5 本 | investigate、design_review_a、design_review_b、design_decide、applier → lassdas-investigate、lassdas-design-review-a、lassdas-design-review-b、lassdas-design-decide、lassdas-applier |
| 観測・配送 | chain.e2e_profile と chain.deliver は空。consumer と矛盾する組合せは起動前に拒否 |

`internal/runtime/config.go` の Load と BuildServices の双方を通す。見本のコピーだけでは空の report_destinations と runner 既定が残る。常用 tracker bot の鍵は runtime services が読む既存 env へ設定する (変数名の正本は `internal/runtime/services.go`)。

LASSDAS_RUNTIME_CONFIG=/etc/lassdas/config/runtime.json、LASSDAS_STATE_DIR=/data、HERMES_KANBAN_DB=/data/kanban.db、LASSDAS_AGENT_TREE_ROOT=/data/runs を揃える。**HERMES_KANBAN_BOARD は runtime.hermes_board と同じ値を必ず生成する。** runtime はその設定名の板へカードを作るが、entrypoint の dispatch は env 未指定で別の既定名を使うためである。C は boot・受付・dispatch が同じ板を使うことを検査する。TARGET_GITHUB_TOKEN は entrypoint が target-token へ移す。

### 5.3 秘密ファイルの検査

init は次の既存設定を渡す。

```text
LASSDAS_GUARDED_FILES=/data/secrets/target-token:/data/secrets/board-pass:/data/secrets/board-tracker-key:/data/route.key
```

board-tracker-key は任意の板の操作用鍵を設定した場合だけ存在。必須の target-token と board-pass は存在して関所から読めることも確認。既存 guard は無いファイルを飛ばすため、一覧だけでは不十分。初回にできる route.key は生成後も検査し、以後の boot では同じ一覧で検査する。

既存 guard は関所所有のファイルを 0600 に直す場合がある。拒否試験では補正できる 0644 と、他所有者で補正できず役に読めるファイルを分け、後者で起動拒否を確認。検査を無効にして進めない。

## 6. AC4 — 分離と実測

agentexec は役を uid 2001〜2063 のプールで動かす。関所と別 uid、同時の役同士も別 uid とし、所有者と mode で秘密・home・作業場所を閉じる。**filesystem 全体を隠す仕組みではない。** /etc/passwd 等の公開ファイルは役も読める。

検証コマンドは `internal/worker/validation.go` の専用 HOME/TMP・env 除外・process group・時間制限で走る。別 uid や filesystem/network sandbox の保証はない。関所と同じ uid の検証コードから秘密ファイルを読むことまでは防がない。これは現行 Pod と共通の残余であり、guard 成功を「任意の検証コードから秘密が守られる」証明にはしない。

ホストの利用者と Docker 管理権限のある利用者は env・volume・docker inspect/exec で鍵を読める。同じコンテナでは command line の prompt も見え得る。役の env を絞っても argv は秘匿されない。本人の一時鍵は prompt や argv に載せない。

今回の実測は 2026-09-08、Docker Desktop 4.66.0 (222299)、client 29.4.0/server 29.3.0、arm64。既存の image digest は `sha256:71c5e47364537691c36a55f8665f8c4a72c7e00ad03125e37ae3a3316c32f059`、entrypoint SHA-256 は `821fb6c589ef5d3b4e120fa884709b8b960aa1fc14fef566f159992c420758c0` (作業枝と一致)。外部 API・本番コンテナ・本物の鍵は使用していない。

| 試験 | 結果と限界 |
|---|---|
| file capability と別 uid | cap_setuid/setgid/chown/kill を確認、実際の役は uid 2001/gid 2000 |
| named volume の貸出/返却 | workspace 所有者 1000 → 2001 → 1000。0600 secret は読めず、/etc/passwd は読めた |
| SQLite | WAL commit 後、別コンテナで reopen して値を確認。実際の ledger/kanban の並行負荷は未実測 |
| image 内の道具 | Go/Node/npm/pnpm あり、cargo/make なし。実際の納品先の検証は未実測 |
| Docker 既定の bwrap user namespace | 権限エラー、終了 1。Hermes の既定経路に bwrap は不要。bwrap が必要な役の構成は未対応として止める |
| 実際の entrypoint の拒否 | root 所有 0644 の人工 secret を guard へ渡すと REFUSING TO START、終了 1、常駐前に停止 |
| 非秘密 config だけの read-only mount | host の親 0700/config 0755/JSON 0644 で uid 1000 が読め、書込みは EROFS。tool-pins.txt は見え、mount 外の env は見えない。host の env 0600 と JSON 内容は不変 |
| bind mount の chown/SQLite | 未実測。state には採用しない。上の設定 mount の検査とは別 |

再現は同じ digest の使い捨てコンテナ、network none、新規 named volume、人工の secret/workspace で行う。getcap、agentexec --check、agentexec --workspace … -- id、--reclaim、Python sqlite3 の WAL commit/reopen を記録。bwrap の試験は `bwrap --unshare-user --unshare-pid --ro-bind / / --proc /proc --dev /dev -- /bin/true`。段階 0 の再現記録には版・digest・コマンド・終了値を添える。

## 7. AC5 — モデルと鍵の対応

URL は現行検査と同じ HTTPS、認証情報/query/fragment/明示 port なし、末尾 / なしの正規形。必要な API と応答を疎通で確認し、任意の互換口に無条件対応するとは言わない。特定の接続先や事業者に固定しない。

| 身元 | 関所の直接呼出し | Hermes / env |
|---|---|---|
| 受付・対象導出 | models.implementer.api_key_env=LASSDAS_INTAKE_TARGET_KEY | 直接のみ。モデルは実装役と同じ |
| 実装役 | implementer のモデル情報と一致 | lassdas-implementer / LASSDAS_IMPLEMENTER_KEY・MODEL |
| レビュー A | reviewers[a].api_key_env=LASSDAS_REVIEW_A_KEY | lassdas-review-a / LASSDAS_REVIEW_A_KEY・MODEL |
| レビュー B | reviewers[b].api_key_env=LASSDAS_REVIEW_B_KEY | lassdas-review-b / LASSDAS_REVIEW_B_KEY・MODEL |
| 受付・起案 | readiness.assessor.api_key_env=LASSDAS_READINESS_ASSESSOR_KEY | 直接のみ |
| 受付・確認 | readiness.checker.api_key_env=LASSDAS_READINESS_CHECKER_KEY | 直接のみ |
| 調査・設計 | designer.api_key_env=LASSDAS_DESIGNER_KEY | 直接のみ。LASSDAS_DESIGNER_MODEL も一致 |
| 写し役 | agents.applier の起動定義 | lassdas-applier / LASSDAS_APPLIER_KEY・MODEL |

受付は対象導出 + 起案 + 確認の **3 呼出し**。対象導出は models.implementer.api_key_env、実装役は profile を読むため、モデルを追加せず鍵を分けられる。見本の複数役の鍵共有は生成設定へ引き継がない。

既定ではレビュー A/B が設計と候補を両方担当し、design_reviewers と design_reviewer_agents は省略。同じ役の工程間の鍵再利用は許すが、別身元同士の同値は拒否する。設計レビューを別モデルにする場合は 2 名とも指定し、新しい鍵 2 本を入力。LASSDAS_DESIGN_REVIEW_A/B_KEY_VAR は LASSDAS_DESIGN_REVIEW_A/B_KEY を指す。既定では LASSDAS_REVIEW_A/B_KEY を指す。直接 API と profile の両経路を表の対応で検査する。

agents は既存の `hermes --profile <name> -z`、secret_env のキー名は profile の api_key_env と一致。モデル・vendor・base_url も同じ回答から生成。agent の timeout はカード壁より短くする (初期値は実装 3600、レビュー 3600、写し 900 秒)。調査・設計は直接 API と既存予算を使う。

**agents.reviewer_agents は A/B 全員分生成し、各 profile と secret_env を設定する。** 省略すると ReviewerAgentFor が共通の agents.reviewer へ戻るため、カード側の profile 指定だけでは鍵は分かれない。入力 URL は全 endpoint の base_url と **LASSDAS_GATEWAY_BASE_URL** の両方へ同値で保存する。

全身元の疎通には既存 ModelInvoker.Preflight を B の検査処理から使い、表の直接呼出しと profile が使う実際の鍵を一つずつ渡す。現行 worker preflight の endpoint 選択は implementer/readiness/reviewers に限られるため、その CLI の単純な繰り返しでは designer・写し役・実装 profile の鍵を検査できない。出力には身元名と合否のみを残し、鍵は記録しない。

`internal/worker/config.go` の検査を呼び、独自の不完全なコピーで代用しない。レビュー 2〜4 名・2 vendor 以上・ID 重複なし・(base_url, model) 重複なし・実装役/設計役と同じ組は各最大 1 名、設計レビューは全員か無し・同じ ID 集合・2 vendor 以上・モデル重複なし、受付 2 段の vendor 相違、endpoint/lens/出力上限を検査。init 初版はレビュー 2 名。未知の vendor は聞き、gateway ホスト名から会社を断定しない。

## 8. AC6 — 設計モードと動作確認の完了

design.default=on、trigger_terms は空、probe は空、写し役と設計 profile 5 本を必ず生成。`internal/attendant/chains.go` の検査は写し役未設定をカード作成前に止めるが、受付の費用までは防がないので init が起動前に検査する。

動作確認は自然文の小変更。init が許可ファイルの現行内容から、1 ファイルに固定文言を 1 箇所追加する等の検算できる提案を作り、利用者が確認してから起票。既存内容を消す提案や依頼書式の強制はしない。対象・期待差分を台帳に残し、PR の repo/base/head/差分と照合する。

POST 前に相関 ID と予定内容を台帳へ保存し、依頼へ照合用の印を添える。API 応答前の切断では成否未知として同じ project・起票者・相関 ID で検索し、未確認のまま再 POST しない。issue ID と delivery ID を引き継ぎ、再開/redo で二重起票しない。新しい動作確認を明示したときだけ新しい相関 ID を使う。

既定の待機は **30 分で表示を返す**が、完了保証や本体を殺す期限ではない。超過は「進行中・init 未完了」、再実行で同じ依頼を追う。失敗・回答待ち・予算不足も既存状態と対処を示す。`internal/runtime/chain.go` の 1 巡の壁は調査 40、設計レビュー 70×2、判定 5、写し 20、候補レビュー 70×2、検証 60、納品 60、合計 465 分。再レビュー・受付・dispatch 待ちもあり、30 分を全体上限にはできない。

完了条件は PR の実在と期待差分の一致、設計/候補レビューと検証の合格、当該 delivery の写し役確定記録。起動時の別 uid 実測結果も台帳から参照する。ただし現行 AgentRun に実効 uid 欄はない。**個別の写し役記録が実効 uid を証明するとは書かない。** このためだけに記録プロトコルを拡張しない。

## 9. 実装 issue の単位

| 単位 | 内容 | 依存・受入 |
|---|---|---|
| A: cli 契約 | §4 の kind、GitHub/報告先契約、root ファイル範囲、観測/配送の不実行、回帰 | なし。§10 段階 0。範囲表現と kind は独立に検査できるなら同じ issue の別 PR |
| B: init | §1〜3・7 の対話、外部取得した鍵の検査、生成設定、台帳/再開、ソース版入口 | A の設定形、C の起動関数。対話部は並行可 |
| C: run | named volume、mount、起動/停止/状態/ログ、§5 の検査と板 | A と独立。既存形式の人工設定で先に確認 |
| D: 動作確認 | 本人の一時鍵、相関 ID、二重起票防止、待機再開、PR 機械照合 | A/B/C。段階 2 |
| E: 文書 | README の入手/起動、RUNTIME_POD のローカル方式、TICKET_AUTHORING の CLI と自由文 | 段階 1 で入手/起動を納品、段階 2 で確認手順を追記 |

本設計の issue は未起票。関連は調査・設計役 (issue #18、上記設計書) と [別ユーザでの役の実行](RUNTIME_POD.md) (issue #23)。承認後に A〜E のリンクを追記。1 変更=1 PR、独立レビュー、既存 CI、squash の順。設計判断や共有 image に効く変更は独立レビュー 2 系統。

## 10. 段階導入と受入 (DoD)

| 段階 | 合格条件 |
|---|---|
| 0: 契約と環境 (A/C) | cli の consumer/report_destinations が origin/release/workflow 無しで本体読込を通る。root の main.go を明示範囲で扱える。既存 web fixture を無変更で読み、拒否条件・配送動作を維持。cli で観測/配送を呼ばない。§6 の局所試験を再現し、実際の ledger/kanban 並行読書きと再起動も検査 |
| 1: 立ち上げ (B/C/E) | §1.1 の前提を揃えた macOS/arm64 の新規 project で設定の手編集なしに起動。起票者・検証場所・秘密保護の 3 ブロッカーを解消。中断/失効/再開で重複資源なし。板の認証成功/未認証拒否、秘密欠落/漏えい時の未完了。入力欄数と時間を記録 |
| 2: PR (D/E) | 小変更が既定の設計工程から PR へ到達し期待差分・記録に一致。30 分以内は測定目標、超過は値と工程を記録。中断から同じ依頼へ復帰。外部操作は対象と内容の確認後 |
| 再現性 | 独立した新規 project/volume で段階 2 を 3 回連続通過。digest、時間、入力欄数、工程、役別費用を私的な受入記録へ。既存状態の削除を前提にしない |

今回完了したのは §6 の局所実測。cli 実装、本体全体の正の起動、本人の鍵での起票、モデル疎通、最初の PR、3 回受入は未実施。公開設計に資格情報・顧客識別子・金額を記載しない。

中止/再検討条件: web 互換性が壊れる、必要な分離が既定 Docker で成立しない、初期 PR が 3 回連続で成立しない場合は、根拠と最小修正範囲を示して再検討。native 方式や共有 release の変更へ自動で移らない。実装差分 10 ファイル以上、層/権限境界の拡大、レビュー修正の増殖ではスコープを再検証し、継続・分割・不要分の撤回を明示する。共有基盤・権限・本番への拡大は本設計の承認では許可されない。

## 11. 決定と出どころ

2026-09-08 の発注者との会話で決定: init 一本で最初の PR まで、最初は単純な CLI repo、ローカル Docker と Pod 共通 image、既存トラッカー、Hermes、URL と役ごとの鍵、設計を評価・提示した後に公開操作を承認、init は本番担当と別担当。追加決定: トラッカーや GitHub の鍵は外で取得して入力。

設計モード on は「改修は調査して設計してから直す」という方針による。named volume、閲覧用の板、待機 30 分、ソース版入手は本版の設計提案であり、個別の発注者決定と混同しない。

## 12. 未決定・未実測

- 共通 image の取得可能な参照・digest・ソース SHA の対応は既存配布者の情報が必要。新しい公開配布基盤を実装の前提にしない。
- 実際の初回 repo/モデルの質問数、入力時間、PR 時間、費用は §10 で測る。
- daemon 登録、複数 project 同時利用、image 更新コマンドは初版外。run start は Docker 常駐、stop は対象だけを停止し状態を保持。
- 別 uid での検証等、現行 Pod の検証分離強化は本書の外。既存保証の限界は §6。

## 13. スコープ外

GitHub Issues の入口、Docker 無しの native 実行、project ごとの Kubernetes、web の init、本番反映/データ/設定の変更、共有 release/CI/CD/registry/権限/統治の変更、鍵の発行、受付の再試行変更は扱わない。消費側固有の repo・接続先・事情は入力として受け取り、フレームワークへ埋め込まない。

## 14. 回帰セット

| 失敗 | 固定する試験 | 場所 |
|---|---|---|
| cli が web 必須項目で止まる / web が変わる | consumer/GitHub/report route の両種類、cli の配送設定拒否 | A |
| root が扱えない / 範囲が広がる | main.go 完全一致、main.go.bak/隠れた名前/symlink/隣接範囲の拒否、既存 prefix 不変 | A |
| bot 起票が受付に弾かれる | 本人 ID 不一致は POST 前停止、許可者不変、一時鍵再入力 | D |
| ホストだけの道具で検証済みになる | image の版/実行結果、道具欠落・コマンド失敗・候補 bytes 変更の拒否 | B/A |
| 秘密欠落/漏えいでも起動成功 | 実在、関所/役の可読性、capability 不足、guard 補正可能/不可能 | C |
| model 設定と実際の鍵がずれる | 8 身元の同値拒否、直接 API/profile/env 一致、設計レビュー全員/省略、失効再入力 | B |
| 重複操作 / 未完了を完了と呼ぶ | 中断・POST 応答喪失・30 分超過・redo で同じ issue/PR を追跡 | B/D |
| ログだけで成功を誤認 | 板の認証・本体の生存、PR の repo/base/head/差分と確定記録を実際に照合 | C/D |

新規の読み取り専用評価役が、ブロッカー・完全性 30/一貫性 25/精密性 20/実現可能性 15/追跡可能性 10・実コード突合の 3 層で評価。review の目安は 80/100 以上・ブロッカー 0。改善後の再評価は最大 2 回とし、評価を繰り返すために範囲を増やさない。
