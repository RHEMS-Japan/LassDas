---
name: lassdas-migrate
description: 手元の docker にセットアップした LassDas を、常時動く場所 (Kubernetes や EC2) へ移す。「PC を閉じると止まる」「K8s に移したい」「常時動かしたい」と言われたら使う
---

# 手元の LassDas を常時動く場所へ移す

手元の docker で動かしている本体は、**Mac が寝れば止まり、Docker Desktop を終了すれば自動では戻らない** (起動時に再起動の指定を付けていない)。盤面も `127.0.0.1` に固定して公開しているので、その端末の上でしか見えない。常時動かすなら場所を移す。

## 0. いまどの状態かを見分ける (ここから始める)

手順が 3 通りに分かれる。**先にこれを確かめる。**

```
docker ps -a --filter name=lassdas-<project>
ls ~/.lassdas/<project>/
```

| 見えたもの | 状態 | この後 |
|---|---|---|
| コンテナがあり Up | **動いている** | 1 節から順に全部 |
| コンテナが無い / Exited、`init.json` はある | **まだ動かしていない・止まっている** | 1 節を飛ばし、4 節 (台帳を写す) も飛ばしてよい。置いて起動するだけ |
| `init.json` が無い | **台帳が消えている** | 移設の前に `lassdas-repair` で復旧する |

真ん中の状態はよくある。`setup.json` の `host` に値を書いたまま `apply` すると、本体は「別の場所に置く」と読んで**準備だけで止まる**ので、一度も起動していない。その場合、移すべき台帳の中身は空で、実質は「K8s に新しく立てる」になる。

## 1. 現物と置き先を確かめる

```
lassdas run spec --project <project>
```

これが正本。何を・どういう条件で置けばよいか (image / 実行ユーザー / platform / 板の port / 環境のファイル / 設定 / 消してはいけない書き込み先) が全部入っている。**推測で書かない。**出力の `before_moving` は、そのまま持って行くと静かに壊れるものの一覧。

置き先ごとの対応 (Secret / ConfigMap / PVC / Service) は `~/.lassdas/SETUP.md` の「置き方の例」にある。

### 1.1 置き先を調べてから、選択肢にして聞く

**どのクラスタのどの namespace に置くかを、自分で決めない。勝手に決め打ちすると、別のクラスタや無関係な namespace に本体が立つ。**手元の kubectl が向いている先がその人の意図とは限らない。

まず実在するものを調べる。

```
kubectl config get-contexts -o name
kubectl --context <選んだ context> get namespace
kubectl --context <選んだ context> get nodes -l kubernetes.io/arch=arm64
kubectl --context <選んだ context> get storageclass
```

そのうえで、**出てきたものだけを選択肢にして**利用者に聞く。白紙で「どのクラスタですか」と聞かない — 何を答えればよいか分からないし、綴りを間違えれば後の工程で弾かれる。何も見つからなければ、そのことを言ってから自由記述で聞く。

確認する 4 つ:

| 聞くこと | 調べ方 | 見つからないとどうなるか |
|---|---|---|
| どの context | `kubectl config get-contexts` | 別のクラスタに立つ |
| どの namespace | `get namespace` | `default` や無関係な場所に混ざる |
| **arm64 の node があるか** | `get nodes -l kubernetes.io/arch=arm64` | **Pod が起動しない** (image は linux/arm64) |
| どの storageClass | `get storageclass` | PVC が Pending のまま止まる |

arm64 の node が 1 つも無ければ、**そのクラスタには置けない**。ここで止めて利用者に伝える。先へ進んでも Pod は Pending のまま動かない。

**以後のコマンドで `--context` と `-n` を省略しない。**省略すると手元の current-context に向かう。この文書のコマンドはすべて両方を明示してある。

## 2. 先に止める (動いている場合だけ)

```
lassdas run stop --project <project>
docker ps --filter name=lassdas-<project>
```

2 行目が空になるまで次へ進まない。**同じ課題管理の project を 2 つの本体が見ると、同じ依頼を二重に処理する。**実際に起きている。

台帳は SQLite の WAL なので、**動いたまま写すと壊れた台帳が届く**。止めるのは二重起動の防止とデータの整合の両方のため。

## 3. 持って行く

`run spec` の項目と 1 対 1 で対応させる。

| spec | 移す先 | 注意 |
|---|---|---|
| `image` | そのまま (digest 固定) | タグに置き換えない |
| `user` (1000:1000) | `runAsUser` / `runAsGroup` | image のファイルがこの uid の持ち物 |
| `platform` (linux/arm64) | arm64 の node | x86 の node に置くと起動しない |
| `env_file` | Secret | **値を読まない・会話に出さない。**ファイルごと渡す (6.1 の手順で、ファイルごと渡す) |
| `config_files` (2 つ) | ConfigMap → `config_mount_path` に read-only | 中身は置き場所が変わっても同じ |
| `data_path` (`/data`) | PVC | **作り直さない。中身を移す** (次節) |
| `board_port` | Service | 公開範囲は利用者に確認する |

鍵のファイル (`/data/secrets/...`) は本体が起動時に環境から自分で作る。人が作る必要はない。

## 4. 板の認証を変える

セットアップ直後の板は `LASSDAS_BOARD_AUTH=local` で、**loopback の Host にしか答えない**。Service や Ingress 越しに来た要求は 403 になる。Pod は起動して健康そうに見えるのに、誰も盤面を開けない。

移した先では Secret に次を入れる:

- `LASSDAS_BOARD_AUTH=basic`
- `LASSDAS_BOARD_USER` (`:` を含まない名前)
- `LASSDAS_BOARD_PASS` (16 文字以上)

`local` では盤面からの操作も止まっている。`basic` にすると操作が有効になる。

## 5. 台帳と走行の記録を移す (動いていた場合だけ)

`data_path` には台帳・走行中の依頼・回答待ちが入っている。捨てると、**走行中の依頼と回答待ちが消える**。

止めた状態で、docker の volume から取り出して PVC へ入れる。volume 名は `run spec` の `data_name`。

```
docker run --rm -v <data_name>:/from -v "$PWD":/out alpine tar -C /from -cf /out/lassdas-data.tar .
kubectl --context <ctx> -n <ns> cp lassdas-data.tar <一時 Pod>:/tmp/   # PVC を mount した Pod
kubectl --context <ctx> -n <ns> exec <一時 Pod> -- tar -C /data -xf /tmp/lassdas-data.tar
```

展開後、`/data` 以下が uid 1000 の持ち物になっていることを確かめる。

## 6. Kubernetes に置く (ひな形)

`<>` は `lassdas run spec` の出力と、置き先のクラスタの値に置き換える。**鍵は利用者が自分の端末で作る。**AI は値を読まない・書かない・聞かない。

### 6.1 鍵と設定を先に作る (利用者の手番)

板の認証だけは値を差し替える必要がある (4 節)。環境のファイルを一時コピーして直してから渡す。

```
cp ~/.lassdas/<project>/runtime.env /tmp/lassdas-env
# /tmp/lassdas-env を開き、次の 3 行にする:
#   LASSDAS_BOARD_AUTH=basic
#   LASSDAS_BOARD_USER=<: を含まない名前>
#   LASSDAS_BOARD_PASS=<16 文字以上>
# (元からある板の認証の行は、上の 3 行で置き換える。残すと Service 越しに 403 になる)

kubectl --context <ctx> -n <ns> create secret generic lassdas-<project>-env \
  --from-env-file=/tmp/lassdas-env
rm /tmp/lassdas-env

kubectl --context <ctx> -n <ns> create configmap lassdas-<project>-config \
  --from-file=~/.lassdas/<project>/config/
```

作れたことの確認は**件数だけ**を見る (値は出さない):

```
kubectl --context <ctx> -n <ns> get secret lassdas-<project>-env -o jsonpath='{.data}' | tr ',' '\n' | wc -l
kubectl --context <ctx> -n <ns> get configmap lassdas-<project>-config -o jsonpath='{.data}' | tr ',' '\n' | wc -l
```

`run spec` の `env_keys` の件数と一致していること。設定は 2 件 (`runtime.json` と `m1-consumer.json`)。

### 6.2 置き場所と本体

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: lassdas-<project>-data
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 20Gi
  # storageClassName: <クラスタの既定に合わせる>
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: lassdas-<project>
spec:
  replicas: 1            # 2 つ動かすと同じ依頼を二重に処理する
  strategy:
    type: Recreate       # 書き込み先を 2 つの Pod が同時に掴まないため
  selector:
    matchLabels: { app: lassdas-<project> }
  template:
    metadata:
      labels: { app: lassdas-<project> }
    spec:
      nodeSelector:
        kubernetes.io/arch: arm64      # spec の platform: linux/arm64
      securityContext:
        runAsUser: 1000                # spec の user
        runAsGroup: 1000
        fsGroup: 1000                  # これが無いと /data に書けない
      containers:
        - name: lassdas
          image: <spec の image を digest のまま>
          envFrom:
            - secretRef: { name: lassdas-<project>-env }
          ports:
            - containerPort: 9200      # spec の board_port
          volumeMounts:
            - name: config
              mountPath: /etc/lassdas/config   # spec の config_mount_path
              readOnly: true
            - name: data
              mountPath: /data                 # spec の data_path
      volumes:
        - name: config
          configMap: { name: lassdas-<project>-config }
        - name: data
          persistentVolumeClaim: { claimName: lassdas-<project>-data }
---
apiVersion: v1
kind: Service
metadata:
  name: lassdas-<project>
spec:
  selector: { app: lassdas-<project> }
  ports:
    - port: 80
      targetPort: 9200
```

**image は digest のまま置く。**タグに書き換えると、次に中身が変わったとき何が動いていたか言えなくなる。

盤面をどこまで見せるかは利用者に確認する。社外から見せない前提なら Service のままにして `kubectl port-forward` で見る。外から見せるなら Ingress を足す — その場合 4 節の認証は必須。

### 6.3 起動と最初の確認

```
kubectl --context <ctx> -n <ns> apply -f <上の yaml>
kubectl --context <ctx> -n <ns> rollout status deploy/lassdas-<project>
kubectl --context <ctx> -n <ns> logs deploy/lassdas-<project> --tail=50
```

ログに 1 分ごとの `tick` の行が出ていれば、課題管理を見に行っている。出ていなければ 7 節へ進まない。

## 7. 移せたと言える条件

次が全部揃うまで「移した」と言わない。

1. Pod が起動している
2. 盤面が開く (認証つき)
3. **Pod を落として上げ直しても、走行の記録が残っている** (PVC が効いている)
4. 課題管理に新しく依頼を出して、それが拾われる

## 8. 移した後

- `lassdas run start` / `stop` / `status` / `logs` は docker 前提なので**もう使えない**。相当する操作 (見る・止める・記録を見る) を `.lassdas/progress.md` に、context と namespace と名前が入った**そのまま打てる行**で書く。「kubectl で見る」は書いたことにならない
- `.lassdas/setup.json` の `host` に置いた場所を書く。以後 `setup apply` は、このマシンに立てずに準備だけで止まる
- 手元の docker に残っている止めたコンテナと volume は、移設が 5 節の条件を全部満たすまで消さない

## やってはいけないこと

- 止めずに写す (壊れた台帳が届く)
- `data_path` を作り直す (走行中の依頼と回答待ちが消える)
- 移設先を起動したまま手元のものも動かす
- 環境のファイルの値を読む・会話に出す・書き写す
