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
- ドラッグ移動 / ホイールズーム
- ユーザーごとの旗（16x16・16色パレット、Turso `users` テーブル）。ブラウザの秘密トークンの SHA-256 先頭 8 byte が公開 ID
- 旗は立てた本人のみ外せる（所有者記録以前の旗は誰でも可）
- ユーザー名（16文字まで）と簡易ランキング（安全マスを開く +1（連鎖の広さに関係なく1回）／地雷を開くと -10（地雷が少ないほど増える）、旗は点数なし、上位10人、変化時に全員へ push、画面右に常時表示）
- 旗を立てたマスは立てた人の色の枠で目印が付く
- 旗にマウスを乗せると持ち主の名前と旗の絵を表示（マウス操作のみ）
- 他プレイヤーの選択中マスを表示（変化時にクライアント→サーバー→同チャンク閲覧者へ即時中継、非永続）
- レート制限: 接続ごと＋ユーザーごと＋IPごとのトークンバケット（ユーザー/IPのものは再接続・複数接続でリセットされない）、1IP 5接続まで（`CF-Connecting-IP`）、認証は1接続1回、操作は認証必須
