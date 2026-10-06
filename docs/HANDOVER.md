# 現在地と引き継ぎ

更新: 2026-10-04。これはソースの構成と確認手順であり、稼働先への反映完了の記録ではない。

## 正本と入口

- 本体は [rewrite/README.md](../rewrite/README.md)。受付・役・実行・納品・再開の現在の設定と観測をそこへ集約する。
- 導入は [START.md](../rewrite/START.md)、Kubernetesは [SETUP.md](../deploy/ticket-engine/SETUP.md)、隔離と必要条件は [RUNTIME.md](../rewrite/RUNTIME.md)。
- 製品判断は [PRODUCT_DIRECTION.md](PRODUCT_DIRECTION.md)、導入設計の決定一覧は [INIT_DECISIONS.md](INIT_DECISIONS.md)。設計上の合意と実装済みの動きを混同しない。
- 本体のソースはrewrite内に置いたまま。最上位への移動や改名はこの撤去には含めない。

## 旧本体を撤去した範囲

最上位の旧Go module、旧runnerのコード・設定・知識ひな形・導入CLI・再利用workflow・起動/反映スクリプト・専用の運用設計と画面見本を削除した。古いソースはGit履歴にあり、互換入口や設定/台帳の変換機能は追加していない。

imageは新本体のbundleだけを組み立てる。CIもrewriteのbuild/vet/Go/Python、識別子走査、最小Go版、imageの起動確認を対象とする。UID 1000と導入者向けの既存ツールは残す。旧agent launcherと専用ユーザ群は出荷しない。

配布案内 [DISTRIBUTION.json](DISTRIBUTION.json) は新しい導入手順も読むので残す。mainのimage workflowが匿名取得を確認した後に更新する。ローカルで値を捏造せず、特定の稼働先の鍵や設定をここへ埋め込まない。

## 確認する順序

1. [最上位README](../README.md) のbuild/vet/Go/Pythonを実行し、終了コードを確認する。共有PCでは並列数を制限する。
2. PRのimage-checkで新しいimageが組み立ち、既定の起動コマンドが新本体を起動することを確かめる。
3. 導入先で設定・権限・隔離・通信・納品先を確認する。実行は運用者の許可を得る。ソース撤去はこの許可ではない。
4. 小さな試験依頼を受付から合意した納品と結果確認まで流す。ローカル試験やCIの成功を、実モデルや本番の受入へ読み替えない。

## 運用者に残る確認

既存の旧image、Pod、受付、外部workflow、保存データはこの変更では操作していない。旧main参照の再利用workflowや削除されたCLIに依存する運用は、そのまま最新版へ切り替えられない。稼働先の停止・切替・データの処置は独立に確認し、未完了の依頼が黙って失われないようにする。自動移行は提供しない。

ソースの撤去と既存環境の撤去を別々に記録する。現在の設定での再起動・外部操作の結果確認は必要な機能であり、旧形式の互換維持を目的とした二重実装とは分ける。
