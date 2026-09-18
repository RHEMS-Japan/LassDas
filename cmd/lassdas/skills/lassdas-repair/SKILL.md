---
name: lassdas-repair
description: LassDas の setup が止まった・同じ失敗を繰り返している・.lassdas のファイルを消してしまった ときに使う。原因を読む順番と、消した後の戻し方
---

# LassDas が止まったとき

**止まったら、設定を書き換える前に理由を読む。**`setup.json` を変えて再実行する、を繰り返してはいけない。1 回が数分かかり、どれが効いたか分からなくなり、まだ動いている本体がある状態で二重に立ち上がる事故につながる。

## 1. 消してはいけないもの

| 場所 | 中身 | 消すとどうなるか |
|---|---|---|
| `~/.lassdas/<project>/init.json` | 台帳。どの段まで通ったか・固定した image・docker の向き先・課題管理の ID・各役のモデル・板の port | **`lassdas run` が全部使えなくなる** (「この project の init 台帳がありません」)。動いているコンテナは止まらないまま、CLI から見えなくなる |
| `~/.lassdas/<project>/runtime.env` | 鍵 | 利用者に入れ直してもらうことになる (`lassdas setup secrets`) |
| `~/.lassdas/<project>/config/` | 本体に渡す設定 | `setup apply` のやり直しで作り直される |
| `<repo>/.lassdas/setup.json` | 回答 | 調査と決定事項が全部消える。最初からやり直し |

台帳と鍵は別ファイルなので、**台帳を消しても鍵は残る**。

## 2. 台帳 (`init.json`) を消してしまったら

この順で戻す。順番を変えない。

1. **先に、動いている本体を探して止める。**台帳が無いと `lassdas run stop` は使えない。docker で直接見つける:
   ```
   docker ps --filter name=lassdas-<project>
   docker stop <出てきた名前>
   ```
   これを飛ばすと、次の手順で立ち上げた 2 つ目が同じ課題管理の project を見て、同じ依頼を二重に処理する。
2. 回答 (`<repo>/.lassdas/setup.json`) と鍵 (`~/.lassdas/<project>/runtime.env`) が残っているか確かめる。
3. `lassdas setup apply --project <project>` を実行する。**全段やり直しになる** (どこまで通ったかの記録が消えているため)。回答と鍵が残っていれば、利用者の出番は増えない。
4. 通ったら `lassdas run start --project <project>` で起動し、板が見えることを確かめる。

## 3. `apply` が止まったときの読み方

出力の最後に出ている**段の名前**を見る (prepare / consumer / tracker / models / runtime / smoke)。そのうえで:

### 納品先検査 (consumer) が止まったとき

検査はこの順で進み、**手前で止まったら奥は一切実行されていない**:

1. 設定の形 → 2. 検証ディレクトリ → 3. git の取り出し → 4. **道具のバージョン一致** → 5. install / verify コマンド → 6. 基準コミットが変わっていないか → 7. Git 管理下のファイルが変わっていないか

- **verify コマンドを何に変えても同じように落ちるなら、4 より手前で止まっている。**verify をいじるのをやめる
- 4 は `setup.json` の `toolchain` に書いた版と、image の中の実際の版の一致を見る。実際の版はこれで出る (image は `~/.lassdas/distribution.json` の `image`):
  ```
  docker run --rm --entrypoint go <image> version
  ```
  他の道具は `--entrypoint <コマンド名> <image> --version`
- `scope` に wildcard は無い。ディレクトリ接頭辞 (末尾 `/`) と root ファイルの完全一致だけ。`*.go` はどのファイルにも当たらない
- 7 は install / verify が Git 管理下のファイル (`go.sum` など) を書き換えたときに落ちる。ビルドの生成物は許される

### 準備はできたのに本体が起動しないとき

`setup.json` の `host` に値があると、本体は「別の場所に置く」と読み、設定と鍵を用意するところで止まる。**このマシンの docker で動かすなら `host` は空**。選択肢の見出し (「このマシン」など) をそのまま書き写すと、このマシンを選んだのに起動しない状態になる。

## 4. やってはいけないこと

- 理由が分からないまま `setup.json` を書き換えて再実行する
- 台帳・鍵・回答のファイルを消す (「作り直せばいい」で消さない)
- 動いている本体を確認せずに 2 つ目を起動する
- 止まった段を飛ばして次に進む
