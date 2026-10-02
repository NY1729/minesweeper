# OpenWorld Minesweeper

## 構成

- `frontend/` — Vite + TypeScript + Canvas。`dist/` を Cloudflare Pages / Workers Static Assets に配置。
- `backend/` — Go の authoritative game server。VPS 上で Docker 実行。
- `Turso` — 開いたマス・旗などの chunk 状態を永続化。
- 地雷位置は `WORLD_SEED + 座標` からサーバー側で決定論的生成し、未公開の地雷情報はクライアントへ送らない。

## Backend

1. `backend/.env.example` を `backend/.env` にコピー
2. Turso URL / Token、`WORLD_SEED`、`ALLOWED_ORIGINS` を設定
3. `docker network create edge`
4. `docker compose up -d --build`

Cloudflare Tunnel から同じ `edge` network の `http://backend:8080` にルーティングする想定。

Endpoints:
- `GET /api/health`
- `WS /api/ws`

## Frontend

1. `frontend/.env.example` を `frontend/.env` にコピー
2. `VITE_WS_URL=wss://<backend-domain>/api/ws`
3. `npm install`
4. `npm run build`
5. `frontend/dist/` を Cloudflare にデプロイ

## 現在の仕様

- 無限座標
- 32x32 chunk
- マルチプレイヤー共有盤面
- WebSocket による reveal / flag のリアルタイム同期
- 0 マスの flood reveal
- Turso への dirty chunk 定期保存
- ドラッグ移動 / ホイールズーム / スマホは2本指ピンチで拡大縮小、長押しで旗（長押しで文字選択やメニューが出ないよう抑止）
- ユーザーごとの旗（16x16・RGB各16段階（4096色）で自由に選べるピクセルエディタ（ペン/消しゴム/スポイト、太さ1〜3）。保存形式は1ピクセル4桁の16進（旧256文字形式も読める）、Turso `users` テーブル）。ブラウザの秘密トークンの SHA-256 先頭 8 byte が公開 ID
- 旗は立てた本人のみ外せる（所有者記録以前の旗は誰でも可）
- ユーザー名（16文字まで）と簡易ランキング（安全マスを開く +1（連鎖の広さに関係なく1回）／地雷を開くと -10（地雷が少ないほど増える）、旗は点数なし、上位10人、変化時に全員へ push、画面右に常時表示）
- 旗を立てたマスは立てた人の色の枠で目印が付く
- 旗にマウスを乗せると持ち主の名前と旗の絵を表示（マウス操作のみ）
- 他プレイヤーの選択中マスを表示（クライアントは変化時に送信 → サーバーが0.1秒ごとに閲覧者ごとへ1通にまとめて転送、保存なし）
- レート制限: 接続ごと＋ユーザーごと＋IPごとのトークンバケット（ユーザー/IPのものは再接続・複数接続でリセットされない）、1IPあたりの同時接続数に上限（既定20、`MAX_CONNS_PER_IP`、`CF-Connecting-IP`で判定）、25秒ごとにpingして70秒無応答の接続は切断（寝落ちしたスマホ等が接続枠を占有し続けないように）、認証は1接続1回、操作は認証必須

## 負荷テスト

サーバー上で、バックエンドのコンテナに直接つないで実行する（Cloudflare を通さない）。

```bash
scripts/loadtest.sh -n 300 -dur 60s        # 300人、それぞれ別の場所で遊ぶ
scripts/loadtest.sh -mode crowded -n 100   # 全員が同じ場所を見る（最悪ケース）
scripts/loadtest-cleanup.sh                # テストで Turso に書いたデータを削除
```

- 擬似プレイヤーが、プロフィール保存・旗・選択マスの移動を繰り返す。結果は接続数、受信数、旗の往復時間、バックエンドの CPU/メモリ。
- 既定は本番向け: 旗だけ操作（点数が動かずランキングに影響しない）、原点から遠い座標を使用。それでも Turso に `load-N` という利用者とチャンクが書かれるので、終わったら `loadtest-cleanup.sh` を実行する。
- 本番で動かすときは少人数（`-n 50`）から始め、利用者の少ない時間帯に行う。同じサーバーの CPU を使うため、本物のプレイヤーにも影響する。
- Cloudflare 経由では、同じIPからの接続数に上限（既定20）があり、`CF-Connecting-IP` の偽装も拒否される。大人数の負荷は測れない。
- ツール本体は `backend/cmd/loadtest`。
