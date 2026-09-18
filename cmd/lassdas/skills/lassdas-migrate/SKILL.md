---
name: lassdas-migrate
description: いま手元の docker で動いている LassDas を、常時動く場所 (Kubernetes や EC2) へ移す。「PC を閉じると止まる」「K8s に移したい」と言われたら使う
---

# 動いている LassDas を移す

手元の docker で動かしている本体は、**Mac が寝れば止まり、Docker Desktop を終了すれば自動では戻らない** (起動時に再起動の指定を付けていない)。盤面も `127.0.0.1` に固定して公開しているので、その端末の上でしか見えない。常時動かすなら場所を移す。

## 0. まず現物を出す

```
lassdas run spec --project <project>
```

これが正本。何を・どういう条件で置けばよいか (image / 実行ユーザー / platform / 板の port / 環境のファイル / 設定 / 消してはいけない書き込み先) が全部入っている。**推測で書かない。**出力の `before_moving` は、そのまま持って行くと静かに壊れるものの一覧。

置き先ごとの対応 (Secret / ConfigMap / PVC / Service) は `~/.lassdas/SETUP.md` の「置き方の例」にある。

## 1. 先に止める (順序を変えない)

```
lassdas run stop --project <project>
docker ps --filter name=lassdas-<project>
```

2 行目が空になるまで次へ進まない。**同じ課題管理の project を 2 つの本体が見ると、同じ依頼を二重に処理する。**実際に起きている。

台帳は SQLite の WAL なので、**動いたまま写すと壊れた台帳が届く**。止めるのは二重起動の防止とデータの整合の両方のため。

## 2. 持って行く

`run spec` の項目と 1 対 1 で対応させる。

| spec | 移す先 | 注意 |
|---|---|---|
| `image` | そのまま (digest 固定) | タグに置き換えない |
| `user` (1000:1000) | `runAsUser` / `runAsGroup` | image のファイルがこの uid の持ち物 |
| `platform` (linux/arm64) | arm64 の node | x86 の node に置くと起動しない |
| `env_file` | Secret | **値を読まない・会話に出さない。**ファイルごと渡す (`kubectl create secret generic <name> --from-env-file=<env_file>`) |
| `config_files` (2 つ) | ConfigMap → `config_mount_path` に read-only | 中身は置き場所が変わっても同じ |
| `data_path` (`/data`) | PVC | **作り直さない。中身を移す** (次節) |
| `board_port` | Service | 公開範囲は利用者に確認する |

鍵のファイル (`/data/secrets/...`) は本体が起動時に環境から自分で作る。人が作る必要はない。

## 3. 板の認証を変える

セットアップ直後の板は `LASSDAS_BOARD_AUTH=local` で、**loopback の Host にしか答えない**。Service や Ingress 越しに来た要求は 403 になる。Pod は起動して健康そうに見えるのに、誰も盤面を開けない。

移した先では Secret に次を入れる:

- `LASSDAS_BOARD_AUTH=basic`
- `LASSDAS_BOARD_USER` (`:` を含まない名前)
- `LASSDAS_BOARD_PASS` (16 文字以上)

`local` では盤面からの操作も止まっている。`basic` にすると操作が有効になる。

## 4. 台帳と走行の記録を移す

`data_path` には台帳・走行中の依頼・回答待ちが入っている。捨てると、**走行中の依頼と回答待ちが消える**。

止めた状態で、docker の volume から取り出して PVC へ入れる。volume 名は `run spec` の `data_name`。

```
docker run --rm -v <data_name>:/from -v "$PWD":/out alpine tar -C /from -cf /out/lassdas-data.tar .
kubectl -n <ns> cp lassdas-data.tar <一時 Pod>:/tmp/   # PVC を mount した Pod
kubectl -n <ns> exec <一時 Pod> -- tar -C /data -xf /tmp/lassdas-data.tar
```

展開後、`/data` 以下が uid 1000 の持ち物になっていることを確かめる。

## 5. 移せたと言える条件

次が全部揃うまで「移した」と言わない。

1. Pod が起動している
2. 盤面が開く (認証つき)
3. **Pod を落として上げ直しても、走行の記録が残っている** (PVC が効いている)
4. 課題管理に新しく依頼を出して、それが拾われる

## 6. 移した後

- `lassdas run start` / `stop` / `status` / `logs` は docker 前提なので**もう使えない**。相当する操作 (見る・止める・記録を見る) を `.lassdas/progress.md` に、context と namespace と名前が入った**そのまま打てる行**で書く。「kubectl で見る」は書いたことにならない
- `.lassdas/setup.json` の `host` に置いた場所を書く。以後 `setup apply` は、このマシンに立てずに準備だけで止まる
- 手元の docker に残っている止めたコンテナと volume は、移設が 5 節の条件を全部満たすまで消さない

## やってはいけないこと

- 止めずに写す (壊れた台帳が届く)
- `data_path` を作り直す (走行中の依頼と回答待ちが消える)
- 移設先を起動したまま手元のものも動かす
- 環境のファイルの値を読む・会話に出す・書き写す
