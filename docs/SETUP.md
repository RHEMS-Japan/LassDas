# LassDas の導入 — 開発 AI への共通指示

この文書は、利用者から「LassDas をこのプロジェクトに導入して」と頼まれた開発 AI (Claude Code、Codex など、ファイルとコマンドを扱える道具を持つもの) が読む。利用者は隣にいて、決めることに答え、鍵を入れる。AI は調べ、整理し、聞き、書き、動かし、確かめる。

全体方針は [PRODUCT_DIRECTION.md](PRODUCT_DIRECTION.md)、個々の判断は [INIT_DECISIONS.md](INIT_DECISIONS.md) (本文の A01 などはその項目番号)。この版で導入できる範囲は末尾の「この版でできること」を先に読む。

## 聞き方 — 自由記述で投げない

**利用者への質問は、調べた結果から作った選択肢で出す。**「どこで動かしますか」「どのモデルにしますか」と白紙で聞かない。何を答えればよいか分からないし、綴りを間違えれば後の工程で弾かれる。

手順は毎回同じ:

1. **そのマシンと repo を調べて、実際に選べるものを列挙する**
2. **推奨を 1 つ決めて、理由を 1 行で添える**
3. **選択肢として出す。**利用者は選ぶだけ。どれにも当てはまらなければ自由記述で答えてもらう

選択肢はコードにも文書にも書いていない。**その場で調べて作る。**固定の一覧を持つと、一覧に無いものが選べなくなる。

例 — `host` (どこで動かすか):

```
docker context ls        → このマシン / 登録済みのリモート docker
kubectl config get-contexts → 到達できるクラスタと namespace
```

見つかったものを並べて、「このマシンの docker (推奨: いちばん短く始められる)」「K8s の <context> / <namespace>」のように出す。**何も見つからなければ、そのことを言ってから自由記述で聞く。**

**このマシンを選んだら、`host` には何も書かない (空にする)。**`host` に値があると、本体は「別の場所に置く」と読み、`apply` は設定と鍵を用意するところまでで止まり、起動しない。選択肢の見出しをそのまま `host` に書き写すと、このマシンを選んだのに起動しない状態になる。

例 — モデル。**役ごとに 1 つずつ聞かない。**「実装役はどれにしますか」を 6 回やると、利用者は組み合わせの良し悪しを判断できないまま 6 回答えることになる。

まず 1 つだけ聞く: **品質を優先するか、費用を抑えるか、その中間か。**

そのうえで、**6 役ぶんが埋まった組み合わせを 2〜3 案、表にして出す。**案ごとに、1 依頼あたりの費用の目安と、何が変わるかを 1 行で添える。

```
案 A (品質優先)          実装: <上位モデル>  審査 A: <別会社の上位>  審査 B: <さらに別会社>
                         受付: <上位>        確認: <別会社>          写し: <実装と同じ>
                         目安 $0.7〜1.0 / 依頼。指摘の質が上がる

案 B (中間・推奨)        ...  目安 $0.3〜0.5 / 依頼。実装は上位、審査は中位

案 C (費用優先)          ...  目安 $0.1〜0.2 / 依頼。速いが、見落としが増える
```

**規則を満たした案だけを出す。**満たしていない案を出すと、後の検査で弾かれて選び直しになる:

- 審査 A と審査 B は別のモデルで、別の提供会社
- 受付の判定役と確認役も別の提供会社 (同じモデルを 2 度指名するなら 1 人扱いで規則の対象外)
- 実装役と同じモデルを使えるレビュー役は 1 つまで

**モデル名をこの文書に固定で書かない。**書いた時点で古くなる。**いま実際に使えるモデルの中から、あなたが選んで案を作る。**

利用者が案を選んだら、そのまま 6 項目に展開して `setup.json` に書く。**個別に変えたい役があれば、そこだけ聞き直す。**

例 — 課題管理: Backlog の接続先が分かるなら (既存の設定、`~/.lassdas` の別 project、環境変数)、それを第 1 候補にする。project キーは、その space で見えるものを列挙できるなら列挙する。

**答えが 1 つしかないものは聞かない。** `git remote` から納品先が一意に決まるなら、確認だけして次へ進む。

## 0. 守ること

- **repo への書き込みは `.lassdas/` だけ。** 既存の文書・コード・CI は調査のために読む。調査コマンドが lockfile や設定を生成する副作用も含めて守る。既存ファイルの変更が必要なら、理由と変更案を示して別の開発作業にする (B03 B04)。
- **鍵の値を読まない・書かない・聞かない。** 鍵は利用者が `lassdas setup secrets` で入れる。`.lassdas/` にも会話にも鍵の値を出さない (G05)。
- **できていないことを済んだと言わない。** 設定を書いただけでは導入完了ではない。小さな試験依頼 1 本が、合意した納品と確認まで通って完了 (I01)。
- **聞くのは決められないことだけ。** 一問一答、答えやすい選択肢つき。すでに読める情報を聞き直さない。「任せる」と言われたら、推奨と理由を示して確定してもらう (B08)。
- **外部の権限変更は個別に承認を得る。** クラウドや repo の権限を変える案は、内容と影響を示し、その変更への承認をもらってから行う。導入全体への OK と区別する (B05)。
- **足りない道具・権限は具体的に示して止まる。** 代わりに人にやらせて「済み」にしない (A06)。

## 1. 始める前に、機械で確かめる

0. 本体の CLI と案内: 1 台につき 1 回、本体 repo の checkout の中で `go build -o lassdas ./cmd/lassdas && ./lassdas setup install` を実行する (引数なし。配布者の案内は同じ repo の `docs/DISTRIBUTION.json` から読まれる。別の案内を使うときだけ `--note PATH` か個別の引数)。それが CLI を `~/.lassdas/bin/lassdas` に、この文書と本体 repo の docs/ にある文書 (参照先を含む) を `~/.lassdas/` に、配布者の案内 (本体イメージ・本体ソースの SHA・ビルド記録・本体 repo) を `~/.lassdas/distribution.json` に、開発 AI の skill を `~/.claude/skills/lassdas-setup/` に置く。以下の `lassdas …` はその CLI で呼ぶ。`setup.json` の `image` `engine-sha` `build-record` `engine-repository` は案内から自動で埋まるので書かなくてよい。install がまだなら、本体 repo を取得して上のとおり組み立てて実行する。納品先 repo の中では CLI を実行するだけで、本体 repo の中身を納品先に持ち込まない。
   image の取得に失敗したときは、表示された理由で次が決まる: registry に拒否された (denied) なら案内の `registry_login` の手順を利用者に実行してもらう (無ければ配布者に image の公開設定を確認する)。ネットワークで途中で切れたなら認証の問題ではないので、接続を確認して同じコマンドを再実行する (取得済みの層は再利用される)。registry に存在しない (digest 不一致) なら案内が古いので install をやり直す。
   本体を新しくするときは、本体 repo を取得し直して `./lassdas setup install` をやり直してから `lassdas setup apply` を実行する。apply は案内の image がこの本体のものと違えば「案内が新しくなっています」と述べて prepare からやり直し、コンテナを新しい image で入れ替える。何も言わずに古いままにはしない。
1. 道具: `git`、`go` (本体の CLI をソースから組み立てる)、`docker` (Docker Desktop が起動していること)。`lassdas setup check` が無いもの・動いていないものを示す (G02)。
2. 対象: `git remote -v`、いまの branch、`git status` の未保存の変更、fork や worktree かどうか。名前や現在のディレクトリだけで対象を決めない (A05)。
3. すでに `.lassdas/` がある → `.lassdas/progress.md` を読み、7 段の続きから再開する (J01)。本体が動いている (`lassdas run status --project <name>`) → 「参加する (何も変えない)」「設定を変える」「旧設定を読んで引き継ぐ」を利用者に選んでもらう。動いている本体を止めない (A04 I07)。

## 2. 調べる

repo を読んで、次を埋める。分かったことは根拠 (ファイルと行、PR 番号) つきで控える。

| 知りたいこと | 見る場所 | 回答表の項目 |
|---|---|---|
| 任せる範囲 (どのディレクトリを触ってよいか) | ディレクトリ構成、アプリの数 | `scope` |
| PR の宛先の枝と枝の運用 | README / CONTRIBUTING / AGENTS.md / CLAUDE.md、最近マージされた PR の宛先 (`git log --merges`、`gh pr list --state merged`) | `branch` |
| 依存の入れ方、テストの動かし方、道具の版 | go.mod / package.json / Makefile / CI (`.github/workflows`) | `install` `verify` `toolchain` `verify-directory` |
| 課題管理 | 利用者に確認 (Backlog の URL と project キー) | `tracker-origin` `tracker-project` |
| 本体イメージ | `~/.lassdas/distribution.json` (install が置いた配布者の案内。元は本体 repo の `docs/DISTRIBUTION.json`)。無ければ install が未実行なので、本体 repo を取得して 1.0 のとおり install する | `image` `engine-repository` `engine-sha` `build-record` (案内から自動) |

文書と実態が食い違っていたら (README の PR 宛先と最近の PR の宛先が違う、など)、設定と履歴で経緯を調べる。判断できなければ、相違点と理由つきの推奨を利用者に確認する。名前から推測しない (B02)。

## 3. 決める事項を整理する

7 つの事項それぞれを「調査で分かった事実」「理由つきの推奨」「利用者の意図が要る点」に分ける。該当しない事項は理由を添えて「該当なし」にする (方針書 §3、B08)。

| 決める事項 | 調べて相談する内容 |
|---|---|
| 依頼の入口 | どこから依頼を受け、誰の依頼と回答を扱うか |
| 作業の範囲と根拠 | 対象、参照する知識、触ってよい範囲、守る制約 |
| 開発と納品の進め方 | 作業の起点、既存の手順、PR の宛先、どこまで進めるか |
| 確認方法と完了条件 | 何をどこで確かめるか。成功・未確認・未達をどう区別するか |
| 自律性と人の判断 | 自動で進める範囲、必要な判断、判断する人 |
| 進められない場合の対応 | 誰が何を確認し、利用者に何を伝え、どう再開・終了するか |
| 実行に必要な準備 | 接続先、道具、鍵、実行環境、利用上限 |

## 4. 聞く

決められないことだけを、一つずつ、選択肢つきで聞く。典型的な質問と、答えの行き先:

| 質問 | 行き先 |
|---|---|
| 誰の依頼を受けるか (起票を許可する本人) | `creator-id` (D03)。`lassdas setup secrets` が鍵の持ち主の ID を表示するので、本人ならそれを書く |
| 課題管理の鍵が起票者本人のものでよいか (コメントと状態更新が本人名義になる) | `requester-key-ok` (利用者の承認) |
| 受付のカテゴリ・状態が無いとき、課題管理の project に作ってよいか | `tracker-create` (利用者の承認。鍵に作成権限が無ければ利用者が画面で作る) |
| PR の宛先の枝。マージは誰がするか | `branch`。マージの担当は `agreement.md` (E02 E03 E05) |
| 検証は何を・どこで・いつ・何をもって合格とするか | `verify` と `agreement.md` (F01 F02 F07) |
| 自動で進める範囲と、依頼 1 件の完了地点 | `agreement.md` (E08 I04) |
| 役ごとのモデル (品質と費用の希望を聞き、推奨を出す) | `<役>-model` (H01) |
| 費用・時間・修正回数の上限 | `agreement.md` (H04。この版では本体の既定値で動く) |
| 外部モデルに送ってはいけない path | `agreement.md` (H07。この版では本体は機械で縛らない。その旨を伝える) |
| 本体の置き場所 (受付を誰と共有するか) | この版はローカルのコンテナ = 1 人の試し用 (G01) |

## 5. `.lassdas/` に書く

3 つのファイルを作る。鍵の値は入れない。

- `setup.json` — 本体が読む回答。形は `{"answers": {"<項目>": 値}}`。値は文字列か、配列・真偽値・数値の JSON。項目は末尾の回答表。go.mod も package.json も無い repo では、本体が `scope` `toolchain` `install` `verify` を提案できないので必ず書く (道具や依存導入が不要なら `toolchain` と `install` は `[]`)。
- `agreement.md` — 人が読む合意。進め方、完了地点、自動で進める範囲、人が判断する場面、マージの担当、検証の合格条件、上限、送ってはいけない path、既存文書の在り処 (参照先。全文を写さない)。本体がまだ機械で守れない項目は「本体は未対応・人が守る」と明記する。
- `progress.md` — 済んだこと、未解決、次に誰が何をするか。会話を閉じても別の AI が続きから再開できるように書く。

書いたら `lassdas setup check` を実行し、不足があれば直す。check は回答の不足を示すだけで、何も動かさず、鍵も聞かない。`creator-id` だけは 6 の鍵の入力で持ち主の ID が分かってから書くので、この時点では不足のままでよい (鍵の入力後にもう一度 check する)。

## 6. 環境を整える

0. 本体イメージは公開レジストリ (ghcr.io) にあり、通常はログイン不要。案内 `~/.lassdas/distribution.json` に `registry_login` があるときだけ、利用者が自分の端末で先にそのコマンドを実行する (AI は実行しない。案内にパスワードは含めない決まりで、install が代表的な形を弾く)。
1. 利用者が実行: `lassdas setup secrets --project <name>` — 納品先 GitHub のトークン、Backlog の API キー、OpenRouter の API キー (役ごとに分ける設定ならその本数) を入れる。`~/.lassdas/<name>/` に 0600 で保存され、repo にも会話にも出ない。終わりに Backlog の鍵の持ち主 (名前と利用者 ID) が表示される。起票する本人がその人なら `creator-id` にその ID を書き、鍵を本人名義で使うことの承認 `requester-key-ok` を利用者にもらう。
2. AI が実行: `lassdas setup apply --project <name>` — 回答から設定を組み立て、repo と枝の実在、編集範囲と検証コマンドのイメージ内での試走、課題管理の接続と受付カテゴリ・状態、各役のモデルの疎通 (少額の API 利用料がかかる)、本体の起動と起動時検査、を順に通す。止まったら、出力が示す不足 (回答・鍵・承認) を直して再実行する。済んだ段は飛ばして続きから再開する。ある段の回答を変えるときは `--redo <段>` (prepare / consumer / tracker / models / runtime)。
3. 本体はローカルのコンテナで動く。板は `http://127.0.0.1:<board-port>` (認証なし)。

## 7. 試す

1. 利用者が実行: `lassdas setup smoke --project <name>` — 済んだ段の確認 (名義や疎通) にいくつか答えたあと、本人の鍵 (保存しない) で、1 ファイルに 1 行足す小さな依頼を起票し、PR ができるまで見届ける。
2. AI は結果を確かめる: PR が実在し、依頼の内容と差分が合っていること。板の記録に受付・実装・レビュー・検証が残っていること。
3. 通ったら `progress.md` に「導入完了」と、確認した範囲を書く。通らなかったら原因を調べ、`.lassdas/` の範囲で直して再確認する。プロジェクト側の修正は別の開発作業、本体の未対応は要望として残す (I05 L02)。試せなかった工程は「未検証」と明記し、確認済みの範囲だけで使い始める (I04)。

## 8. 引き継ぐ

`progress.md` に、現在地、済んだこと、未解決、次に誰が何をするか、を残す。利用者の操作が要るときは、理由・現在地・具体的な操作・操作後の再開方法を書く (J07)。

**最後に、依頼を出す人へ [OPERATING.md](OPERATING.md) を渡す。**そこに、拾われる 3 条件・質問の答え方と期限・状態が表示であって入力ではないこと・止め方が書いてある。これを渡さないと、設定は正しいのに「立てたのに動かない」「答えたのに進まない」で詰まる。書き方は [TICKET_AUTHORING.md](TICKET_AUTHORING.md)。

## 回答表 (`.lassdas/setup.json` の項目)

必ず書くもの (本体には既定が無い):

| 項目 | 意味 | どう決めるか |
|---|---|---|
| `repository` | 納品先 repo (owner/name) | git remote から読める。fork や別 remote なら確認する |
| `branch` | 取り込み枝 (PR の宛先。default branch とは限らない) | repo の規則と最近の PR の宛先から確かめる |
| `tracker-origin` | Backlog の接続先 URL (`https://<space>.backlog.com`) | 利用者に確認 |
| `tracker-project` | Backlog の project キー | 利用者に確認 |
| `host` | **このマシン以外**に置くときだけ書く。自由記述 (例: `社内の EC2 (docker context: ops-tokyo)`、`K8s の rin-base / lassdas namespace`)。**このマシンの docker で動かすなら空にする** — 値があると `apply` は準備だけして起動しない | そのマシンで実際に使える置き場所を調べ、**選択肢にして出す** (上の「聞き方」)。このマシンを選んだら空のままにする |
| `creator-id` | 起票を許可する本人の Backlog 利用者 ID (数値) | `lassdas setup secrets` が鍵の持ち主の ID を表示する。別の人が起票するならその人の ID を利用者に確認 |
| `model-base-url` | モデルの接続先 (OpenAI 互換の base URL)。省略すると OpenRouter | 使える接続先を**選択肢にして出す**。既定で名前が出るのは OpenRouter (`https://openrouter.ai/api/v1`) と Cheaper Inference (`https://api.cheaperinference.com/v1`)。他も OpenAI 互換なら URL を書けば動く。**project を作った後は変えられない** (保存した鍵はその接続先のもの) |
| `implementer-model` `review-a-model` `review-b-model` `readiness-assessor-model` `readiness-checker-model` `designer-model` `applier-model` | 各役のモデル名 (接続先が使う名前、例 `anthropic/claude-sonnet-4`) | 品質と費用の希望を聞いて推奨を出し、利用者が確定 |

配布者の案内 (`~/.lassdas/distribution.json`、`lassdas setup install` が置く) から自動で埋まるもの。書けば上書きできる:

| 項目 | 意味 |
|---|---|
| `engine-repository` | 本体イメージの元ソース repo (owner/name) |
| `image` | 本体イメージ (registry/name@sha256:digest)。タグ名から推定しない |
| `engine-sha` | そのイメージに対応する本体ソースの 40 桁 SHA |
| `build-record` | イメージと SHA の対応の記録の URL。通常は、そのイメージを作って push した GitHub Actions の実行 (main の image workflow が `docs/DISTRIBUTION.json` を書く) |


## どこで動かすか (`host`)

**本体は置き場所を知りません。**`lassdas run spec` が「何を動かす必要があるか」だけを出すので、導入を進めている担当が、利用者の答えた場所に置く。

```
lassdas run spec --project NAME
```

出るもの (秘密の値は入らない):

| 項目 | 意味 |
|---|---|
| `image` / `engine_sha` | 固定の digest と、その元になったソースの commit |
| `user` / `platform` | `1000:1000` / `linux/arm64`。イメージ内のファイルの持ち主がこれ |
| `board_port` | 盤面が出る、instance 内の port |
| `env_file` / `env_keys` | 環境を置いたファイルと、そこにある名前。**値は出ない** |
| `config_dir` / `config_files` / `config_mount_path` | 読み取り専用で置く 2 ファイルと、置き場所 |
| `data_path` / `data_name` | 唯一の書き込み先。**再起動をまたいで残る必要がある** (台帳と走行の記録がここ) |

**置くのはあなたの仕事です。** 利用者に manifest を書かせない。資格情報がそのマシンにあるなら (kubectl の context、クラウドの鍵、ssh)、**自分で置いて、動いていることを確認し、管理の仕方を記録する**まで終わらせる。無いなら、何が足りないかを 1 行で言って利用者に頼む。

置いたら必ず 3 つ確認する:

1. **動いている** — その仕組みのやり方で 1 プロセス動いていること
2. **盤面が見える** — `board_port` に到達できること。URL を利用者に渡す
3. **`data_path` が残る** — 再起動して、台帳と走行の記録が消えていないこと。ここを使い捨てにすると、走行中の依頼と回答待ちが失われる

置き方の例:

- **このマシン / リモートの docker**: `lassdas run start` がそのまま使える。リモートは `docker context` を向けておく (linux/arm64 の daemon であれば製品は問わない)
- **Kubernetes**: `env_file` から Secret、`config_dir` から ConfigMap、`data_path` に PVC、`board_port` に Service、あとは Deployment 1 つ。`user` と `platform` は必ず写す
- **それ以外 (EC2 に直接、ECS、systemd など)**: 同じ 6 項目を、その仕組みのやり方で満たす

**`data_path` を使い捨てにしない。**消えると、走行中の依頼と回答待ちの記録が失われる。

**盤面は `board_port` を公開しないと見えない。**どこまで公開するかは利用者に確認する (このマシンなら `127.0.0.1` 固定、共有するなら到達できる場所へ)。

`run status` / `run logs` / `run stop` は docker を前提にしている。**docker 以外に置いたら、その 3 つに相当する操作を `progress.md` に実際のコマンドで書く** — 「kubectl で見る」ではなく、context と namespace と名前が入った、そのまま打てる行を残す。次に触る人 (人でも AI でも) がそこから辿れるように。

**導入そのものには docker が要る。** 上の検証工程は、導入マシンのディレクトリをイメージに渡して動かす (鍵の固定値の読み出し、納品先の clone、設定の検査)。クラスタからはそのディレクトリが見えないので、この工程だけは K8s に置き換えられない。**docker のあるマシンで導入し、instance は好きな場所に置く**、が正しい順序。

実装・適用・レビューは、モデルの設定 (`models`) と実行基盤の役 (launch) の両方に現れる。実際に動くのは launch の方で、鍵は役ごとの `LASSDAS_<役>_KEY` を読む。費用の集計は両方の鍵を対象にするので、同じ鍵を全役で共用している設定では 1 行にまとまり、役ごとに鍵を分けている設定では行が分かれる。

モデルの組み合わせには本体の規則がある。`lassdas setup check` が同じ規則で先に見る:

- `review-a` と `review-b` は別のモデルで、別の提供会社。`implementer` と同じモデルにできるレビュー役は 1 つまで (`designer` も同じ)。
- `readiness-assessor` と `readiness-checker` は、**別のモデルを指名するなら**別の提供会社。同じモデルを 2 度指名してもよく、そのときは 1 人が自分の答えを読み返す形になるので、この規則は当たらない。
- 提供会社はモデル名の接頭辞から本体が判定する (openai / anthropic / google / deepseek / x-ai / meta-llama / mistralai / qwen / moonshotai / z-ai / cohere / amazon)。それ以外の接頭辞は `<役>-vendor` に会社名を書く。
- **接頭辞を使わない接続先 (Cheaper Inference など、`gpt-…` `deepseek-…` のような名前) では、`<役>-vendor` が全役で必須。**判定材料が無いので、書かないと `setup check` が止める。どのモデルがどの会社のものかは接続先の一覧で確認する。
- `separate-design` を true にしたら `design-review-a-model` `design-review-b-model` も必須で、同じ規則 (別モデル・別会社)。

書かなければ本体の提案で進むもの (提案は apply の出力に「本体の提案」として残る):

| 項目 | 意味 | 既定 |
|---|---|---|
| `scope` | 編集を許すディレクトリ接頭辞と root ファイル (JSON 配列) | repo の構成から提案 |
| `toolchain` | 道具と必要な版 (JSON 配列: binary, version, strip_v_prefix) | go.mod / package.json から提案 |
| `install` | 依存導入コマンド (JSON 配列) | 同上 |
| `verify` | 検証コマンド 1〜4 本 (JSON 二重配列) | 同上 |
| `verify-directory` | 検証する repo 内ディレクトリ | repo の root |
| `max-files` `max-file-bytes` `max-total-bytes` `max-changed-lines` `max-changed-bytes` | 1 依頼の変更の上限 | 本体の既定 |
| `category` | 受付のカテゴリ名 | `自動処理` |
| `status-0` `status-1` `status-2` `status-3` | 自動処理中 / 回答待ち / 納品済み / 要確認 に対応する状態名 | 既定あり。無ければ確認後に作る |
| `separate-model-keys` | 役ごとに別の API キーを使うか (true/false) | false (1 本を共用) |
| `separate-design` | 設計レビューを別の 2 モデルにするか (true/false) | false |
| `design-review-a-model` `design-review-b-model` | 設計レビューのモデル名 | `separate-design` が true のとき必須 |
| `tracker-create` | 受付のカテゴリ・状態が無いとき、課題管理の project に作ってよいか (true/false) | 利用者に確認してから書く。無ければ apply がその場で止まる |
| `requester-key-ok` | 自動処理の鍵が起票者本人のもので、コメントと状態更新が本人名義になってよいか (true/false) | 利用者に確認してから書く。無ければ apply がその場で止まる |
| `<役>-vendor` | モデルの提供会社 | モデル名から提案 |
| `board-port` | 板を 127.0.0.1 で開く port | 9200 |

## この版でできること・できないこと

- できる: 納品先が GitHub の repo、課題管理が Backlog、モデルの接続先が OpenRouter、本体は手元の Docker (Apple Silicon / linux-arm64 のイメージ)、納品は PR まで。CLI アプリや文書・スクリプトの repo に向く。
- できない (未対応): デプロイと画面での確認まで含む納品 (Web アプリ)、GitHub Issues からの受付、サーバやコンテナ基盤で共有する本体の起動、`.lassdas/agreement.md` の合意を本体が機械で守ること (上限・送ってはいけない path・マージの担当)。これらは `agreement.md` に「本体は未対応」と書き、未検証の範囲として扱う。
- 共有する本体 (Pod) の運用は [RUNTIME_POD.md](RUNTIME_POD.md)。

## 記録するもの

導入のたびに `progress.md` に残す: 聞いた質問の数、利用者の手作業、かかった時間、モデルの費用、詰まった所と抜け方。次の導入と本体の改善に使う。
