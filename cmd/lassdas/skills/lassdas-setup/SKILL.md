---
name: lassdas-setup
description: LassDas をいまのプロジェクト (repo) に導入する。「LassDas を導入して」「LassDas をこのプロジェクトに入れて」と頼まれたら使う
---

# LassDas の導入

あなたは、利用者がすでに使っている開発 AI として LassDas をこの repo に導入する。手順は 1 つの文書に全部書いてある。

1. まず `{{HOME}}/.lassdas/SETUP.md` を最初から最後まで読む。これが正本。
2. 本体の CLI は `{{HOME}}/.lassdas/bin/lassdas`。文書の `lassdas …` はこのパスで呼ぶ。
3. 配布者からの案内 (本体イメージ・本体ソースの SHA・ビルド記録・本体 repo) は `{{HOME}}/.lassdas/distribution.json` にある。`setup.json` の `image` `engine-sha` `build-record` `engine-repository` は本体がそこから自動で埋めるので、書かなくてよい。非公開レジストリの認証コマンド (`registry_login`) があれば、それは利用者が自分の端末で実行する。あなたは実行しない。
4. 文書の「守ること」を守る: repo への書き込みは `.lassdas/` だけ、鍵の値を読まない・書かない・聞かない、できていないことを済んだと言わない、聞くのは決められないことだけ (一問一答・選択肢つき)。
5. 利用者の出番は 2 回だけ (`lassdas setup secrets` と `lassdas setup smoke`)。その番になったら、実行するコマンドと、そのとき聞かれること (鍵の種類、取得先) を伝えて待つ。
6. 進捗は `.lassdas/progress.md` に残し、会話を閉じても別の AI が続きから再開できるようにする。
