# 調査・設計役 — 実物を計ってから、直し方を決める係 (設計書 v0.5、issue #18)

発注者決定 (2026-09-04): **調査と設計は 1 つの役にする。実装は別の軽い役にする。**
理由は一文で言える — 動いているシステムの不具合や改修では、コードを読むだけの調査は推測であって実測ではなく、推測の上に建てた設計は実装役に忠実に間違えさせる。だから「計る」と「決める」は同じ頭で連続して行い、「写す」だけを分離する。

本書は #18 の受入条件 6 点を決める。実装はしない。用語は [TRUST_MODEL.md](TRUST_MODEL.md) に従う (指紋 = SHA-256、確定記録 = 自分の指紋を含む JSON)。

変更履歴: v0.1 (2026-09-04) 初版 → v0.2 (同日) 独立レビュー 2 系統 (敵対レビュー BLOCKER 3 / 3 層ゲート採点 FAIL 52 点) を反映。役を「プロセスを持たないモデル呼び出し」に改め、資格情報を資源単位で定義し、設計書を納品の関所に束縛した。→ v0.3 (同日) 敵対レビューの全 24 件を照合し、v0.2 に残っていた 8 件 (DB の守りの主従・設計レビューの記録形・調査だけの依頼の終端・調査報告のレビュー・レビュー役の入力・設計役の分離・回帰表の固定場所・上限の置き場) を反映。→ v0.4 (同日) v0.2 への再評価 (敵対レビュー 2 巡目 FIXED 6 / PARTIAL 5 / 新規 8、3 層ゲート 78 点・新規 BLOCKER 1) を反映。実測の記録を連鎖値にして巡ごとの調査報告を接頭辞で検算できるようにし、カードとプロファイルを実在の仕組みに合わせ、cookie jar の扱い・会話の予算・「方針の誤り」の機械の信号を定めた。→ v0.5 (同日) v0.3 への敵対レビュー 2 巡目 (FIXED 20 / PARTIAL 4、新規 MAJOR 1 = DB ロールが関数の既定 EXECUTE と `set_config` で破れる) を反映。

## 0. この係が要る根拠 (実測)

| 起きたこと | 何が足りなかったか |
|---|---|
| 「ログインを押してから遅い」型の依頼が、実装役の推測で 5 回失敗した | どの工程が遅いかを計る手段が実装役に無い |
| 実装役が手数上限に達した。500 手のうち 458 手を使い、そのうち 425 手が同じ検索の繰り返しで、ファイルは 1 つも変わっていなかった (RUNTIME_POD.md 回帰表、`deploy/pod/entrypoint.sh` の手数上限の注記) | 「調べる」と「直す」を同じループでやらせた |
| 画面確認の不合格が「ログイン切れ」と診断された。実物を動かすと、ブラウザが起動していない・英語画面・写真の形式、と別の 3 つが先に出た | 設定とコードを読んだだけの診断は、もっともらしいが実測ではない |
| うまくいった判断 (ログインの取り直し・デプロイ直後の旧画面・写真の形式) は全部「実機で 1 回試して確定 → 直す」の順だった | 計ってから決める、を役として持っていない |

## 1. 一枚の絵

```
いま:  受付 (何を・どこを) ─→ 実装役 (どう直すかを考えながら書く) ─→ レビュー 2 名 ─→ 検証 ─→ 納品
                                     ↑ 方針の誤りはコードが出た後に見つかる (一番高い工程で差し戻し)

これから:
  受付 (何を・どこを・設計が要るか)
    │
    ├─ 設計不要 (方針が本文にある小さな変更) ──────────────────────────→ 実装役 (従来どおり)
    │
    └─ 調査・設計役 (関所が動かすモデル呼び出し。道具は probe 1 つ)
          │  出力 1: 調査報告 (実測の一覧つき)      ← 調べるだけの依頼はレビュー A 1 巡のあとここで終わり
          │  出力 2: 設計書 (原因・方針・触るファイル・変更内容・確認方法)
          ▼
       レビュー 2 名が設計書を判定 (コードを書く前に方針を止められる)
          ▼
       写し役 (軽いモデル): 設計書を写す。判断しない
          ▼
       レビュー 2 名: 実装を判定 (設計書との一致は関所が機械検査、動作は従来の観点) ─→ 検証 ─→ 納品
```

関所 (AI ではない Go プログラム) は、調査報告と設計書も確定記録にし、写し役の指示・実装の確定記録・納品ゲートを設計書の指紋で束縛する。信用するのは関所だけ、という原則はそのまま。

## 2. 登場人物と用語

| 登場人物 | 正体 | 役目 | 信用するか |
|---|---|---|---|
| **調査・設計役** | AI 1 人 (重いモデル)。**プロセスを持たない**: 関所がモデルを呼び、モデルは `probe` という道具を頼むことしかできない | 稼働環境とリポジトリを読み取り専用で計り、調査報告または設計書を書く | **しない** |
| **写し役** | AI 1 人 (軽いモデル)。従来の実装役と同じ Hermes エージェントだが、設計書つきの依頼専用 | 設計書を写す。判断しない | **しない** |
| 実装役 | 従来どおり | 設計不要の依頼だけを従来どおり実装する | しない |
| 画面確認 | Go プログラム (`browsercheck`。本書では「写真係」と呼ぶ) | 反映後の画面を開き、約束の文言を照合し、写真を残す | 関所の一部 |

用語: 本書で「実測」= 関所が probe を実行して残した記録 1 件 (`measurement`)。同じものを別の語で呼ばない。

### 2.1 なぜ「プロセスを持たない」形にするか (最終形との関係)

TARGET_SHAPE.md は「実行も Hermes 純正 (ハーネス 1 本)」と定めた。本書はこの役だけを、関所が直接モデルを呼ぶ形 (受付の起案役・確認役と同じ方式) にする。理由:

- この役の価値は「何を計り、何が返ったか」が全部記録されることにある。汎用エージェント (端末・ファイル書き込み・ブラウザ・ネットワークを持つ) に道具を絞って持たせる形では、①端末から直接コマンドを打てるので probe カタログは境界にならない ②`~/.aws/config` の `credential_process` や `~/.kube/config` の `exec`、`~/.psqlrc`、`~/.curlrc` に自分のコマンドを書いてから正規の probe を呼ぶ、という設定ファイル注入で固定 argv の関所を破れる ③記録されない測定を混ぜて `measured` を偽装できる (レビュー指摘)。境界を成立させるには役のプロセスを消すのが一番簡単で、一番確実
- 受付は既にこの方式で動いている (`internal/worker` の直接呼び出し、`converseJSON`)。ただし現行の `ChatRequest` には道具の宣言が無く、`converseJSON` は 1 往復 (言い直し 3 回) の関数なので、**その外側に道具呼び出しの反復を新設する** (§3.1)。別のハーネスは足さない
- 分離条件 (TARGET_SHAPE と同形): 調査に「画面を操作して観測する」など probe で表せない手段が要る依頼が連続 2 件出たら、その手段を probe として足すか、役を汎用エージェント + 別 UID (#23) に移すかを再設計する

## 3. AC1 — 道具と、機械の縛り

### 3.1 実行の形

```
関所 (Go、uid 1000、資格情報と設定を持つ)
  ├─ モデル呼び出し (ゲートウェイ経由。道具は probe の 1 種類だけを宣言)
  │     ↑↓ 会話: 「probe X を引数 Y で」→ 関所が実行 → 「結果 (本文の抜粋 + 記録 ID)」
  └─ probe 実行器: カタログの固定 argv に穴の値を埋めて実行。出力・時刻・終了コードを記録
```

- モデルが返せるのは **probe 依頼** か **最終回答 (調査報告 / 設計書の JSON)** のどちらかだけ。JSON の形は受付と同じ機械矯正 (言い直し最大 3 回、`converseJSON`)
- 手数上限: probe 依頼は 1 依頼あたり **60 回**、役の壁時間 **1,800 秒** (runtime.json の `chain.investigate.max_probes` / `max_runtime_seconds`。置き場の規則は §9)。上限で最終回答が無ければ `investigation_incomplete` で正直に終わる
- **会話の予算**: モデルに見せる抜粋の合計は 1 会話 **256 KiB** まで。超えたら古い抜粋から順に「実測 ID + 1 行要約 (関所が出力の先頭 200 文字で作る)」に置き換える。記録は run dir に全文が残るので、役は ID で引用できる
- モデルは結果の**抜粋**を読む (1 件あたり 32 KiB まで)。全文は記録に残り、抜粋しか見ていないことを記録に書く
- リポジトリの読み取りも probe (`repo.list` / `repo.read` / `repo.grep`) で行う。これら `repo.*` は**フレームワーク組み込み**の probe で、消費側カタログ (§3.2) はこれに稼働環境の probe を足す。対象は受付が確定した基線 (`baseline.json` の統合ブランチ SHA) の作業コピー。モデルはファイルシステムに触れない

### 3.2 probe カタログ (消費側が設定で宣言する)

稼働環境の道具は「消費側が設定で宣言した実測の形」だけ。フレームワークは**稼働環境の**形を所有しない (組み込みは `repo.*` だけ。消費側の事情は設定として外から受け取る)。

```json
"probes": [
  {"id": "k8s.pods",   "kind": "exec", "argv": ["kubectl", "--context", "<ctx>", "-n", "<ns>", "get",  "pods", "-o", "wide"]},
  {"id": "k8s.logs",   "kind": "exec", "argv": ["kubectl", "--context", "<ctx>", "-n", "<ns>", "logs", "{{pod}}", "--since", "{{since}}"],
                       "args": {"pod": "^[a-z0-9-]{1,63}$", "since": "^[1-9][0-9]{0,3}[smh]$"}},
  {"id": "aws.describe","kind": "exec", "argv": ["aws", "--profile", "<readonly>", "{{service}}", "{{verb}}", "--output", "json"],
                       "args": {"service": "^(ec2|ecs|eks|elbv2|rds|cloudwatch)$", "verb": "^describe-[a-z-]{1,60}$"}},
  {"id": "sql.read",   "kind": "sql",  "dsn_env": "PROBE_DB_DSN", "args": {"query": "^\\s*SELECT\\b[^;]*$"}, "statement_timeout_ms": 10000},
  {"id": "http.timing","kind": "http", "hosts": ["<allowed host>"], "methods": ["GET", "HEAD"], "returns": ["status", "time_total", "bytes"],
                       "args": {"path": "^/[a-z0-9/_-]{0,80}$"}, "cookies": "observation-jar"}
]
```

- `exec` は **固定 argv + 正規表現で形を縛った穴** だけ。シェルは無い (パイプ・リダイレクト・変数展開・複数コマンドは存在しない)。穴の値は正規表現に一致し、空白・制御文字・`;` を含まない
- `sql` は関所が自分の DB ドライバで接続する (psql を起動しない。`~/.psqlrc` の類は読まれない)。**拡張プロトコルで 1 文だけ送る** (複文は構造的に送れない)。正規表現は `^\s*SELECT\b` で始まり `;` を含まない文だけ (`EXPLAIN ANALYZE` は DML を実行し得るので許さない)。接続直後に `SET transaction_read_only = on` と `statement_timeout` / `lock_timeout` を発行するが、これらはセッション設定で外せる補助であって本体ではない。**本体の守りは DB 側の権限** (§3.3)。接続は probe ごとに使い捨て (再利用しない) で、1 文は `BEGIN READ ONLY` の中で送る。副作用を持つ関数 (`pg_advisory_*`, `pg_notify`, `pg_terminate_backend`, `pg_cancel_backend`, `dblink*`, `lo_*`, `pg_read_*`) と設定を変える手段 (`set_config`, `SET`) は関所が名前で拒否し (補助)、`pg_sleep` のような遅い文は `statement_timeout` が止める
- `http` は関所の HTTP クライアントで、許可ホストだけ・GET/HEAD だけ・パスとクエリは probe ごとの正規表現 (`path`) に一致するものだけ・転送 (3xx) には従わない・応答本文は返さない (`returns` に書いた項目だけ)。私有アドレスとリンクローカル (169.254.0.0/16) 宛の拒否は**名前解決の後、接続する時点**で行う (DNS が私有アドレスを返す場合を含む)。認証後の計測が要るときは、関所が写真係と同じ cookie jar を**読み取り専用**で付ける: `Set-Cookie` は書き戻さない。ID 基盤 (ログイン入口) のホストは許可ホストに入れない (ID 基盤は使用のたびにセッション cookie を差し替えるので、そこへ打つと jar が死ぬ。RUNTIME_POD.md「The observation session」)。応答が jar の cookie 名に対する `Set-Cookie` を返したら、その実測を `rotated` として記録し、以後その依頼ではそのホストへ打たない。**モデルは cookie の値を見ない**
- 出力の上限 (既定 256 KiB / 件) と時間の上限 (既定 60 秒 / 件) を probe ごとに持てる。1 依頼の実測の合計 (run dir に残す分) は 16 MiB まで。チケットへの添付は §4.4 の別上限
- カタログ外の依頼、正規表現に合わない穴、上限超過は **実行せず、拒否として記録** する (モデルには「その形は無い」と返す)

### 3.3 縛り (3 層、本体は 1 層目)

1. **資格情報が書けず、見てはいけないものを返さない** — これが本体。資源と操作の単位で列挙する。動詞の接頭辞 (get/describe/list) で分類してはいけない: `get` は Secret の中身も、オブジェクトの本文も、鍵の発行も返す
   - Kubernetes: 読み取り用 ServiceAccount の Role は **resources を列挙**する。`pods`, `pods/log`, `deployments`, `statefulsets`, `services`, `endpoints`, `events`, `ingresses` に get/list/watch。**`secrets`, `configmaps`, `serviceaccounts/token`, `pods/exec`, `pods/portforward`, `pods/attach` は含めない** (configmaps は設定に秘密が混ざる前提で除外。必要な設定値は消費側が probe の固定 argv に書く)。`pods` / `deployments` の get は pod spec の env 直書き値 (configmaps と同じ危険区分) を返し得るので、カタログの固定 argv は `-o wide` / `custom-columns` / `rollout status` のような**本文を返さない出力形**だけにし、`-o yaml` / `-o json` / `describe` はカタログに載せない。それでも残る露出 (probe の設計ミス) は 3 層目の走査だけで守る残余として受け入れる
   - AWS: 読み取り用ロールのポリシーは Allow を `Describe*` / `List*` と、内容を返さない `Get*` (例: `cloudwatch:GetMetricData`, `elbv2:DescribeTargetHealth`) に絞り、**明示 Deny** を置く: `s3:GetObject`, `dynamodb:GetItem/BatchGetItem/Query/Scan`, `logs:GetLogEvents/FilterLogEvents/StartQuery` (ログ本文)、`secretsmanager:GetSecretValue`, `ssm:GetParameter*`, `kms:Decrypt`, `ecr:GetAuthorizationToken`, `sts:GetSessionToken/GetFederationToken/AssumeRole`, `iam:*AccessKey*`, `rds-data:*`。`Describe*` の包括 Allow の中で設定本文を返すもの (`ecs:DescribeTaskDefinition`, `ec2:DescribeInstanceAttribute` の userData, `lambda:GetFunction`, `lambda:GetFunctionConfiguration` の環境変数) も Deny する。ログ本文が要る消費側は、本文を含まない集計 (メトリクス) か、§8 の範囲で個別に許す
   - DB: **本体 = SELECT だけを GRANT した専用ロール** (表・スキーマの所有物ゼロ、`CREATE` なし、`pg_read_server_files` 等の既定ロール非付与、`dblink` / `postgres_fdw` / `pg_read_file` 系の関数は EXECUTE を REVOKE)。**PostgreSQL は関数の EXECUTE を既定で PUBLIC に与える**ので、SELECT だけの GRANT でも `SELECT writer_fn()` (SECURITY DEFINER で書く関数) は通る。対象スキーマの全関数について `REVOKE EXECUTE … FROM PUBLIC` し、`ALTER DEFAULT PRIVILEGES … REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC` で以後の関数にも効かせる (段階 0 の試験に含める)。GRANT 先は**顧客の内容を含まない列だけを持つビュー**で、表そのものへの権限は与えない。`default_transaction_read_only` / `transaction_read_only` は誰でも `SET` で外せる (PostgreSQL の USERSET) ので、接続時に立てるが守りには数えない。段階 0 で「読み取りユーザーが `SET transaction_read_only = off` を実行できても `UPDATE` が権限エラーで失敗する」ことを実機で固定する (§10)。環境ごとのビューの違いは §8
   - HTTP: 許可ホストの列挙 (§3.2)。cookie jar の値は関所だけが持つ
2. **形の関所** — §3.2 のカタログ検査。費用と早期停止のためで、保証の根拠ではない
3. **秘密の伏せ字は関所の責任** — probe の出力を保存する前に、関所が鍵の形 (`AKIA…`, `csk-…`, `Bearer …`, `-----BEGIN … PRIVATE KEY-----`, JWT の 3 節、cookie 値) を走査し、**検出したら出力を保存せず、依頼を拒否として記録する** (伏せて保存はしない。伏せ字は漏れ検出の最終手段であって、資格情報の列挙 (1 層目) が本体)。Hermes の秘密マスク (`HERMES_REDACT_SECRETS`) は Pod では**無効化されている** (`deploy/pod/entrypoint.sh`、実装役にコードを原文で見せるため) ので、当てにしない

**プロンプトは縛りではない。**「書き込みをしないでください」は書くが、それを守ったから安全なのではない。

### 3.4 資格情報の置き場と、#23 との関係

読み取り用の資格情報 (AWS ロールの引き受け設定・DB の DSN・K8s の SA トークン・cookie jar) は関所のプロセス (uid 1000) だけが使う。Pod では実装役・レビュー役も uid 1000 で動いている (RUNTIME_POD.md「Credentials」: 別 UID 化は未達の Phase-3 関門、issue #23) ので、**同じ UID の別エージェントがこれらのファイルを読める** という露出は残る。本書はそれを次のように扱う:

- 露出しても**書けない** (1 層目) こと、**顧客の内容と秘密を返さない** (1 層目) ことを先に成立させる。したがって漏れても「調査・設計役が見てよいもの」以上は見えない
- #23 (エージェントの別 UID 化) を **段階 2 の前提** にする。段階 0・1 (調査モードだけ) は #23 無しで始めてよい。理由: 段階 1 で同じ UID で動く他の役はレビュー A (調査報告の根拠を判定する。§5) だけで、その露出は今日のレビュー役と同じ。写し役 (リポジトリの内容から指示を受け得る役) が動き始める段階 2 から、別 UID を前提にする

### 3.5 記録 (何を計ったかが後から追えること)

実測は 1 件ごとに `{id, probe id, 埋めた値, 開始/終了時刻, 終了コード, 出力本文 (上限内), 出力の指紋, 抜粋の範囲, 拒否の有無と理由}` を関所が書く。役が要約したものではなく、関所が取った生の出力。ファイルは `measurements.jsonl` (追記のみ・1 行 1 件)。各行は自分の内容の指紋に加えて**前の行までの連鎖値**を持つ (`chain_sha256 = sha256(prev_chain_sha256 + line_sha256)`、先頭は空文字から)。「先頭から N 行」が 1 つの連鎖値で検算できるので、巡をまたいで追記しても前の巡が封緘した接頭辞はそのまま検算できる (§4.1)。

## 4. AC2 — 出力の形式

### 4.1 調査報告 `investigation-N.json` (確定記録。N = 巡)

| 項目 | 中身 | 関所の検算 |
|---|---|---|
| `questions[]` | 何を確かめようとしたか (最大 8) | — |
| `measurements_count`, `measurements_chain_sha256` | この巡が使う実測の件数 N と、先頭 N 行の連鎖値 | 先頭 N 行の連鎖値と一致 (後の巡が追記しても接頭辞は不変) |
| `delivery_id`, `round`, `input_sha256`, `config_sha256`, `tool_sha`, `base_sha` | 他の確定記録と同じ実行への束縛 (どの依頼・どの巡・どの入力・どの関所・どの基線か) | 既存と同じ |
| `findings[]` | `{claim, evidence: [measurement ids], confidence: measured / inferred}` | **`measured` は引用 ID が先頭 N 行に実在し拒否でないこと**。無ければ拒否 |
| `unknowns[]` | 計れなかった・計らなかったこと (「不明は不明と言う」) | — |
| `next` | 調査だけの依頼ならここで終わり。設計に進むならその旨 | — |

### 4.2 設計書 `design-N.json` (確定記録。同じ巡の調査報告に束縛)

| 項目 | 中身 | 関所の検算 |
|---|---|---|
| `investigation_sha256` | 同じ巡の `investigation-N.json` の指紋 | 一致 (別の巡の報告は不可) |
| `delivery_id`, `round`, `input_sha256`, `config_sha256`, `tool_sha`, `base_sha` | 実行への束縛 (§4.1 と同じ) | 既存と同じ |
| `cause` | 原因の一文 + 根拠の実測 ID | `measured` を最低 1 件含む (推測だけの原因は不成立) |
| `approach` | 方針の一文 + 採らなかった案 (1〜3) | — |
| `files[]` | 触るファイルと、各ファイルで何をどう変えるか (箇条書き) | 全部が許可範囲 (`allowed_file_prefixes`) の中。件数が `max_files` 以内 |
| `verification` | §4.3 の形 | §4.3 |
| `blast_radius` | 影響する画面・機能・利用者 | — |
| `not_doing` | やらないこと (依頼の外) | — |

**設計書の写し `DESIGN.md` は関所が `design-N.json` から決定論的に描画する** (役は書かない)。写し役の指示に埋め込むのはこの描画結果と `design-N.json` の指紋。レビューされた物と写し役が従う物が別物になる経路を作らない (TRUST_MODEL 疑い 2 と同型の穴を塞ぐ)。

### 4.3 確認方法 (verification) の 2 形

| 形 | 中身 | 関所が設計時点で検査すること | 最終判定 |
|---|---|---|---|
| **文言** | 画面のパス、出ているべき表示、消えているべき表示 | 出ているべき表示が変更前の対象ファイルに**無い**、消えているべき表示が**ある** (受付と同じ検査) | 写真係 (反映後の画面) |
| **計測** | probe ID、埋める値、閾値 (例: `http.timing` の `time_total` ≤ 3.0 秒) | probe がカタログに実在し、閾値が数値 | 反映後に関所が同じ probe を実行して閾値と比べる (写真係の兄弟。段階 2 の実装範囲) |

「変更後にファイルにある見込み」「画面に見える見込み」は設計時点では機械検査できない。前者は写し役の候補を封緘するときに検査し (既存)、後者は写真係が最終判定する。設計レビュー B の観点に「この確認方法でこの変更を判定できるか」を置く。

### 4.4 頼んだ人に見えるもの

- **調査報告コメント** (marker `investigation`、1 回だけ): findings の要点 (measured / inferred を明示)、unknowns、次の一手。`measurements.jsonl` は添付する (§3.3 の走査を通ったものだけ。添付前に関所がもう一度走査する)
- **調査だけの依頼の終端**: 終端コード `investigated` (PR なし・**成功扱い**・失敗連続保留の streak を切る) を `report_protocol` に足す。コメント本文は 16 KiB 内に収め、`measurements.jsonl` と生出力は添付 API で `measurement-<id>.txt` として付ける (1 件 256 KiB × 最大 16 件・合計 4 MiB。橋の添付上限 8 MiB (`internal/backlog/client.go`) の半分。超える分は本文に「添付を省略」と明記し、指紋つきで run dir に残す)。手数・壁時間の上限で最終回答が無い `investigation_incomplete` は失敗として数える (streak を切らない)
- **実装方針コメント** (既存 marker `plan`) の中身を、設計書の要約 (cause / approach / files / verification) に置き換える。**頼んだ人は、コードが書かれる前に方針を読める**。証跡 (trail、6 KiB 上限) と PR 本文にも「設計の要約」節を足す
- 板 (状態ボード) の段階: `investigating` (調査中) / `designing` (設計中) / `design-review` (設計のレビュー中) を足す。レールの節点「調査」「設計」は受付と実装の間に置く

## 5. AC3 — 設計レビュー (コードの前に方針を止める)

- レビュー役 A/B (別会社。モデルと資格情報は既存のレビュー役の設定を使うが、**プロファイルは design-review 用に別建て**: 現行の `worker.command` はプロファイルごとに `--stage review-a/b` を固定で持つため) が `design-N.json` を判定する。**記録の形は新設する** (既存の `Review` は候補の指紋と `path ∈ target_files` に束縛されているので流用できない): `DesignReview{design_sha256, reviewer_id, vendor, model, lens, verdict: pass | revise, findings[{code, section ∈ {cause, approach, files, verification, blast_radius, not_doing}, message}], review_sha256}` と `DesignDecision{design_sha256, review_sha256s[2], outcome: approved | nonconverged, decision_sha256}` (どちらも §4.1 と同じ実行への束縛の項目を持つ)。pass のとき findings は空、1 人でも revise なら不成立、という規則は既存と同じ。カードは `design-review-a` / `design-review-b` / `design-decide` で、`EnsureChain` が `needs_design` のときだけ作る。冪等キーは `<delivery>:design-review-a:d<N>` (N = 設計の巡)。行き詰まりの質問は `design-impasse-question` (design-N.json + DesignReview ×2 から作る。既存の `impasse-question` は候補前提なので別 verb)
- **作者は自分を審判しない** を設計役にも適用する: 設計役はエージェントではなくモデル呼び出し (`ModelEndpoint`) なので、`separatedAgents` (起動定義の分離) ではなく **`ModelConfig` の「実装役と同一 (baseURL, model) のレビュー役は 1 人まで」の規則を設計役にも適用**する。設計役の鍵はレビュー役の鍵と別に持つ (§9)
- 観点 (lens) は実装レビューと分ける。設定 `models.reviewers[].design_lens`:
  - **A (根拠)**: 原因は実測に支えられているか。`inferred` だけで立っている結論はないか。計るべきで計っていないものはないか
  - **B (方針)**: 触るファイルは必要十分か。副作用の見落としは。確認方法はその変更を判定できるか。より小さい直し方はないか
- レビュー役の入力 = `design-N.json` + `investigation-N.json` + `measurements.jsonl` (生出力つき) + 基線のリポジトリ読み取り。probe は打てない (レビュー役は計らない)
- **調査だけの依頼にもレビュー A (根拠) を 1 巡かける** (B は不要。方針が無い)。関所の検算は「引用した実測 ID が実在し拒否でない」までで、引用の中身が主張を支えているかは A が判定する。revise なら役は報告を `investigation-N+1` として書き直す。巡数の上限は設計と同じ `design_max_rounds` を共有し、上限で pass しなければ報告はチケットに出さず `investigation_nonconverged` で正直に終わる (手数・壁時間の尽きた `investigation_incomplete` とは別の終端。原因が違う)
- 巡数: 消費側設定 `design_max_rounds` (既定 3。既存の `max_stages` と同じ場所に置く)。revise のとき役は既存の実測を消さず (`measurements.jsonl` は追記のみ)、**追加の実測は 1 依頼の残り予算 (60 回・30 分) の中で可** (「計るべきで計っていない」が A の観点なので、計り直せなければ応えられない)。次の巡は `investigate` カードをもう 1 回動かす (入力 = 前の巡の `design-N.json` + `DesignReview` ×2 を `previous-findings` として渡す。#11 と同じ機構)。役は `investigation-N+1.json` (接頭辞 N+1 件で封緘し直す) と `design-N+1.json` を出す。上限でも合意しなければ **正直に停止** し (終端コード `design_nonconverged`。失敗として数える)、争点を発注者への 2 択質問に変換する
- 質問の予算は既存どおり **1 依頼あたり 2 回** で、受付・設計・実装で共有する (先に使った工程が消費する)。設計で 2 回使い切れば実装では質問できず、行き詰まりは失敗で終わる
- 設計レビューの合格 = `design-N.json` の指紋を `design-decision.json` に封緘。以後の写し役の指示・候補の確定記録・納品ゲートがこの指紋を参照する
- **「方針の誤りが実装後に見つかった」の機械の信号** (§10 の DoD と撤退条件が数えるのはこの 3 つだけ): ①実装後レビューの finding code `design-wrong` (設計どおりに写したが動かない・設計の前提が崩れている) を含む revise ②写し役の `design-objection` (§7) ③終端 `design_nonconverged`

## 6. AC4 — 設計を飛ばす条件 (受付が決める)

受付の判定 (`readiness/decision.json`、スキーマ版を上げる) に `needs_design: bool` と `design_reason` を足す。既定は消費側設定 `design.default` (`on` を推奨。§11 で決める)。飛ばすのは次を**全部**満たすときだけ:

1. 依頼本文に「どう直すか」が書いてある (起案役が `approach_in_ticket: true` を返し、根拠として本文の引用を添える)
2. 受付が導出した対象ファイル (`target_files`) が 2 つ以下
3. 依頼本文に、稼働環境の観測を示唆する語が無い。語彙は消費側設定 `design.trigger_words` (例: 「遅い」「たまに」「本番で」「ログに」「原因」「調査」)。フレームワークは既定リストを持たない。**未設定 (空) なら条件 3 は成立しない** (設計を飛ばせない。安全側に閉じる)
4. 依頼の種別が「調査」でない (起案役の `request_kind: change | investigation`)

判定は既存の受付と同じ 2 人体制で機械矯正する: 起案役が `needs_design` を出し、確認役が反対なら **設計あり** に倒す (安全側)。理由はチケットの受付コメントに 1 行で出す (「方針が本文にあるため設計を省略」)。変更行数の見込みは条件に使わない (候補が無い時点では計れない)。

TICKET_AUTHORING.md には「どう直すかを本文に書けば設計を省略できる」を追記する (#22 と一緒に)。

## 7. AC5 — 写し役の契約 (設計書つきの依頼)

- **指示** = `DESIGN.md` (関所の描画) + 守ること。「設計書に書かれていない変更をしない」「`files[]` 以外を触らない (許可範囲をさらに狭める)」「迷ったら止まる」
- **迷ったら止まる** の形: 写し役は run dir に `revise-design.json` (理由・どの項目か) を書いて **exit 0** で終わる (Hermes の契約では exit 0 = エージェント完了)。候補を封緘するカード (現行では review-a カード冒頭の `seal-candidate`) がこのファイルを見つけたら候補を封緘せず `design-objection.json` として確定記録にし、**そのカードを専用の終了コード (`ExitDesignObjection`) で終える** (写し役のカード自体は exit 0 で終わっているので、検知点は封緘するカード)。進行係はこのコードを失敗ではなく「設計に戻す」と分類し、その巡の実装・レビューのカードを退役させ、stage-N を `objection` で閉じて (候補は無い)、設計の次の巡 (`investigate` カード d<N+1>) を作る。実装の巡番号は進めない。異議は設計の巡として `design_max_rounds` を消費する
- **モデルと手数**: 消費側設定 `agents.applier` (軽いモデル、`max_turns` 40)、カードの壁 = `ChainStage.MaxRuntimeSeconds` に 1,200 秒 (40 手 × 平均 20 秒 = 800 秒 + 封緘の余裕。カードの壁は中の予算より長くする、は `internal/runtime/chain.go` が実 2 件の死因として記録した規則)。設計なしの依頼は従来の `agents.implementer`
- **実装後の検算 (関所)**: 候補の確定記録 `candidate.json` に `design_sha256` を入れ、**`candidate.files ⊆ design.files[]`** を封緘時と納品ゲートの両方で機械検査する。納品ゲートには `design-N.json` と `design-decision.json` を引数で渡し (`ValidatePublishGate` の引数を増やす)、候補の `design_sha256` と決定の `design_sha256` の一致・決定の `outcome = approved`・files の包含を検査する。AI に任せない
- **実装後のレビュー (AI)**: 観点は「設計書の各項目が差分に現れているか」「余計な変更がないか」に加え、TARGET_SHAPE の 4 層緩和にある「実際に動かす」観点を**残す** (設計どおりでも動かないことはある)。方針の再審はしない (設計レビューで済んでいる)

## 8. AC6 — 本番データの境界 (単一の出典。#20 はここに従う)

環境ごとに「読んでよいもの」を資格情報 (§3.3 の 1 層目) で分ける。プロンプトで分けない。

| 環境 | 状態・設定・計測 | ログ | 保存されたデータ | 秘密 |
|---|---|---|---|---|
| ステージング | 可 | 可 (本文を含むものも) | 可 (ビュー経由。既定は本番と同じ内容なしビュー。消費側が「ステージングに顧客の内容は無い」と設定で宣言した場合だけ、内容列を持つビューを追加で GRANT できる) | 不可 |
| 本番 | 可 | **本文を含むものは不可** (集計・件数・時刻は可) | **原則不可** (顧客の会話本文・個人情報を含む行)。件数・時刻・ID の存在確認は可 | 不可 |

実現の形 (§3.3 で列挙した資格情報そのもの):

- 本番の DB ユーザーには、件数・時刻・ID だけを持つビューに SELECT を許す。顧客の内容を持つ列はどのビューにも含めない
- 本番のログは、K8s の `pods/log` 権限を本文を含まないコンテナ (例: 進行係・関所) の namespace に限り、アプリ本文を含むログは probe カタログに載せない。AWS 側は `logs:Get*`/`FilterLogEvents` を Deny
- モデルへの実送信本文 (保管されていれば) は本番では読まない (対象バケットに `s3:GetObject` を Deny)
- 秘密はどの環境でも読まない (K8s `secrets` を Role から除外、AWS の秘密系 API を Deny、§3.3 の 3 層目で漏れを検出)

**決めてもらうこと**: 上の表でよいか。特に本番の「件数・時刻・ID の存在確認は可」の線。

## 9. 統合点 (実装 issue に分ける単位)

| 場所 | 変更 |
|---|---|
| `internal/runtime/chain.go` の stage 定数と `ChainStages` | `investigate` (調査報告と、設計ありなら設計書まで **1 カード** で出す。§2 の 1 役 = 1 カード), `design-review-a`, `design-review-b`, `design-decide`, `apply` を追加。`EnsureChain` は `needs_design` のときだけ設計系の 3 枚と `apply` を作る。壁時間は `investigate` **2,400 秒** (役の壁 1,800 秒 + 封緘の余裕。カードの壁は中の予算より長くする — `chain.go` が実 2 件の死因として記録) / `design-review-*` 既存のレビューと同じ / `design-decide` 300 秒 (validate と同形の非 AI) / `apply` 1,200 秒。調査だけの依頼では `investigate` → `design-review-a` (入力は `investigation-N.json` だけ) → `design-decide` の 3 枚 |
| `internal/runtime/config.go` `ChainProfiles` と `deploy/pod/entrypoint.sh` | `ChainProfiles` (現在 5 項目固定) に 5 項目を足す: `investigate` と `design_decide` (validate / publish と同形の非 AI プロファイル。関所のプロセスが動く)、`design_review_a` / `design_review_b` (既存のレビュー役のモデル・鍵を使い、`worker.command` に `--stage design-review-a/b` を持つ別プロファイル)、`applier` (軽いモデル、`LASSDAS_APPLIER_MODEL` / `LASSDAS_APPLIER_KEY`)。調査・設計役のモデルは消費側設定 `models.designer` と `LASSDAS_DESIGNER_KEY` |
| `internal/attendant/budget.go` のロール表 | 調査・設計役と写し役の鍵を予算疎通に加える |
| `internal/worker` 受付 (`readiness.go`, `intake.go`) | `needs_design`, `design_reason`, `approach_in_ticket`, `request_kind` をスキーマ版上げで追加。確認役の検算規則 |
| 新規 `internal/probe` パッケージ | カタログの読み込みと検査、exec / sql / http の実行器、記録 (`measurements.jsonl` の連鎖値)、秘密走査 |
| `internal/worker` 新規 `investigate` | モデルの反復呼び出し (道具 1 種類)、`investigation-N.json` / `design-N.json` の確定記録と検算、`DESIGN.md` の描画 |
| `cmd/worker` | `investigate` 副命令、`design-instruction` (写し役の指示描画)、`seal-candidate` の `design_sha256` 束縛と `revise-design.json` の扱い、`ValidatePublishGate` の files 包含検査 |
| `cmd/worker/agent.go` | 設計レビューのプロンプト (`design_lens`)、写し役のプロンプト |
| `internal/attendant` | 新カードの生成・退役、冪等キーの `:d<N>` (設計の巡) を `ParseChainCardKey` / `chainViewFor` が実装の巡と区別して読むこと、`ExitDesignObjection` の分類と設計巡の再開、終端 `investigated` / `design_nonconverged` / `investigation_nonconverged` / `investigation_incomplete` の扱い、板の段階 |
| `internal/runner/deliver.go` | 計測形の確認 (§4.3): 反映後に関所が同じ probe を実行して閾値と比べ、写真係の判定と同じ場所 (`deliverVerification`) に載せる |
| `internal/hook` | 調査報告コメント (marker `investigation`)、実装方針コメントの設計書要約、終端コード `investigated` / `investigation_incomplete` / `investigation_nonconverged` / `design_nonconverged` (`report_protocol.go`) と streak の扱い (`investigated` だけが streak を切る) |
| `internal/worker/artifact.go`, `impasse.go` | 確定記録 `DesignReview` / `DesignDecision` と検算、`design-impasse-question` |
| 消費側設定 (`config/m1-consumer.json` 例) | `probes[]`, `design.default`, `design.trigger_words`, `design_max_rounds`, `models.designer`, `agents.applier`, `models.reviewers[].design_lens` |
| 上限の置き場 (規則) | **巡数と役の定義** (`design.*`, `agents.*`, `models.*`) は消費側設定。**手数と壁時間** (`chain.investigate.max_probes` / `max_runtime_seconds`, `apply` の `MaxRuntimeSeconds`) は runtime.json の `chain` (既存のカードの壁と同じ場所)。同じ値を両方に置かない |
| docs | TRUST_MODEL.md の登場人物表に 3 行 (調査・設計役 / 写し役 / 画面確認)、RUNTIME_POD.md の住人と回帰表 |

## 10. 段階導入と、定量の受入 (DoD)

| 段階 | 内容 | 受入 (計れる形で) |
|---|---|---|
| 0 | probe 実行器・記録・秘密走査・確定記録の検算を**単体テストで固定** (稼働環境なし)。資格情報 3 本 (AWS 読み取りロール / DB 読み取りユーザーとビュー / K8s 読み取り SA) と probe カタログ。**中身を提示してから適用** | 故意の試験 11 件が全部拒否される: K8s の `secrets` get、`pods/exec`、AWS `secretsmanager:GetSecretValue`、`s3:GetObject`、DB の表への直接 SELECT、`SET transaction_read_only = off` のあとの `UPDATE`、複文 (`SELECT 1; UPDATE …`)、`EXPLAIN ANALYZE DELETE …`、`dblink`、SECURITY DEFINER で書く関数の `SELECT writer_fn()`、HTTP のリンクローカル宛 |
| 1 | 調査モードだけ。調べるだけの依頼を **3 本** 流す (例: 認証後の遅延を工程ごとに計る) | 3 本すべてで調査報告がチケットに付く。findings の `measured` の 100% が実在する実測 ID を引用。probe の拒否を除く失敗 0 |
| 2 | 設計モード + 設計レビュー + 写し役。**#23 を前提**。無害な変更依頼を **5 本** 流す | 5 本のうち、方針の誤りが実装後に見つかる件数 (§5 の 3 信号) **0**。写し役が手数上限 40 に到達した件数 **0**、手数の中央値 **20 以下**。設計 1 巡の費用が実装 1 巡の費用を超えない (実測で記録) |
| 3 | 設計を飛ばす判定を有効化 | 直近 10 本で、飛ばした依頼の実装後差し戻しが **1 本以下**。理由が受付コメントに出る |

**撤退条件** (TARGET_SHAPE の分離条件と同形): 段階 2 以降で、設計あり経路の受入失敗 (§5 の 3 信号のいずれか) が **連続 3 回** 起きたら、`design.default` を `off` に戻し、原因を issue に記録してから再開する。

## 11. 発注者に決めてもらうこと

1. 本番データの境界 (§8 の表、特に「件数・時刻・ID の存在確認は可」)
2. `design.default` を `on` にするか (推奨 `on`。費用は設計 1 巡ぶん増え、実装は軽くなる。§6 の飛ばす条件で小さな依頼は安いまま)
3. 調査・設計役と写し役のモデル (推奨: 調査・設計役 = 実装役と同じ重いモデル。レビュー役 A の既定も同じ重いモデルなので、§5 の分離規則により A か設計役のどちらかを別モデルにする。写し役 = レビュー役 B の会社の軽いモデル)
4. 設計レビューの巡数上限 (既定 3)
5. リリースの起点: 現在は開発枝から Pod を出している。issue 運用に合わせて main に戻す時期
6. 調査だけの報告にレビュー A を 1 巡かける (推奨 on。費用はレビュー 1 巡ぶん。off にすると調査報告は AI レビューなしで発注者判定になる)

## 12. スコープ外と関連

**スコープ外**: 俯瞰役 (#19。走行中の停滞を見る係。本書の役とは別)、エージェントの別 UID 化の実装 (#23。段階 2 の前提として依存するが本書では設計しない)、消費側インフラ (ロール・ビュー・SA) の作成そのもの (段階 0 で提示してから適用)、本番への書き込み (永久にしない)、画面を操作する観測 (probe で表せない。§2.1 の分離条件)。

**関連**: [#17](https://github.com/RHEMS-Japan/LassDas/issues/17) (置き換え元) / [#11](https://github.com/RHEMS-Japan/LassDas/issues/11) (レビュー役と実装役の巡をまたぐ記憶。設計レビューの revise で `previous-findings` として同じ機構を使う) / [#19](https://github.com/RHEMS-Japan/LassDas/issues/19) / [#20](https://github.com/RHEMS-Japan/LassDas/issues/20) (本番観測の境界。**§8 が唯一の出典**で、#20 はこれに従う) / [#22](https://github.com/RHEMS-Japan/LassDas/issues/22) (起票ミスの返し方。§4.3 の文言形の条件を起票者に伝える) / [#23](https://github.com/RHEMS-Japan/LassDas/issues/23) (資格情報の隔離。§3.4)。信用モデル = [TRUST_MODEL.md](TRUST_MODEL.md)、Pod の住人 = [RUNTIME_POD.md](RUNTIME_POD.md)、起票の書き方 = [TICKET_AUTHORING.md](TICKET_AUTHORING.md)。

## 13. 回帰セット (RUNTIME_POD.md の表に足す行)

| 死因 | 固定するテスト | 固定する場所 (package・テスト名) |
|---|---|---|
| カタログ外の probe、正規表現に合わない穴、`;` や空白を含む値、リンクローカル宛の HTTP が実行される | probe 実行器: 実行せず拒否として記録 | `internal/probe` `TestCatalogRefusesOutOfShapeRequests` |
| 複文・`EXPLAIN ANALYZE` の DML・副作用関数が `sql` probe を通る | 1 文しか送れない経路と名前の拒否 | `internal/probe` `TestSQLProbeSendsOneReadStatement` |
| 出力に鍵の形が含まれたまま保存・添付される | 秘密走査: 保存せず拒否として記録。添付前の再走査 | `internal/probe` `TestSecretShapedOutputIsRefused` |
| `measured` の finding が実測 ID を引用していない / 引用先が拒否 / `measurements.jsonl` の指紋不一致 | investigation の検算 | `internal/worker` `TestInvestigationRequiresMeasuredEvidence` |
| 設計書の `files[]` に許可範囲外のパス / `cause` に `measured` が無い / 文言の確認方法が変更前のファイルにある | design の検算 | `internal/worker` `TestDesignValidation` |
| 設計レビューの記録が候補前提の `Review` 検算に落ちる / 設計役と同じモデルのレビュー役が 2 人 | `DesignReview` / `DesignDecision` の検算、`ModelConfig` の同一 (baseURL, model) 規則 | `internal/worker` `TestDesignReviewArtifacts`, `TestDesignerIsSeparatedFromReviewers` |
| 写し役が `files[]` に無いファイルを変えた | `seal-candidate` と `ValidatePublishGate` の包含検査 | `cmd/worker` `TestSealRefusesFilesOutsideDesign`, `internal/worker` `TestPublishGateRequiresDesignSubset` |
| `DESIGN.md` と `design-N.json` が食い違う | 描画は決定論 (同じ入力から同じ出力) をテストで固定 | `internal/worker` `TestDesignRenderingIsDeterministic` |
| 写し役の `revise-design.json` が無視されて候補が封緘される | 封緘段のテスト | `cmd/worker` `TestSealTurnsObjectionIntoDesignRound` |
| 設計巡が上限で合意しないまま写し役に進む | design-decide のテスト | `internal/attendant` `TestDesignRoundsStopAtLimit` (fake Hermes) |
| 起案役が「設計不要」と言い確認役が反対したのに飛ばす | needs_design の機械矯正 | `internal/worker` `TestNeedsDesignFallsToSafeSide` |
| probe の手数上限・壁時間で最終回答が無いのに `ready` 扱いになる | `investigation_incomplete` の終端 | `internal/worker` `TestInvestigationBudgetEndsHonestly` |
| 調査だけの依頼が終端コードを持たず、進行係が納品カードの完了を待ち続ける | `investigated` の終端と板の節点 | `internal/hook` `TestInvestigatedIsATerminalCode`, `internal/attendant` `TestInvestigationOnlyDeliveryRetires` |
| 後の巡が `measurements.jsonl` に追記すると前の巡の調査報告が検算できない | 接頭辞の連鎖検算 | `internal/probe` `TestMeasurementChainVerifiesPrefixes` |
| `design.trigger_words` が空なのに設計を飛ばす | 空は条件不成立 | `internal/worker` `TestEmptyTriggerWordsNeverSkipDesign` |
| `http` probe が私有アドレスに解決するホストへ接続する / jar の cookie を書き戻す / ID 基盤のホストへ打つ | 接続時の判定・読み取り専用の jar・ホストの除外 | `internal/probe` `TestHTTPProbeRefusesPrivateResolution`, `TestHTTPProbeNeverWritesJar` |
| 写し役の異議が失敗として数えられ、設計に戻らない | `ExitDesignObjection` の分類 | `internal/attendant` `TestDesignObjectionReopensDesignRound` |

稼働環境が要るのは段階 0 の「資格情報で拒否される」試験 11 件だけ。上の表は全部、稼働環境なしの単体テストで固定できる。
