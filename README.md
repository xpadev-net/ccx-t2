# ccx-t2

`ccx-t2` は、`task-pr-orchestrator` / `task-pr-worker` の運用を補助するための
Go バックエンドと React Web UI です。単一プロセスで複数プロジェクトのタスク台帳、
worker の tmux セッション、worktree、MCP エンドポイント、PR 状態を扱います。

## 主な機能

- グローバル設定に登録された複数プロジェクトの管理
- `tasks/ledger.md` 形式のタスク台帳の読み書き、作成、更新、分割、削除
- task ごとの git worktree と tmux worker window の管理
- orchestrator / worker 向け MCP HTTP エンドポイント
- worker 完了通知、フォローアップ、PR 状態確認
- WebSocket による台帳、orchestrator ログ、worker ログの更新通知
- React Web UI によるプロジェクト切り替え、タスク、orchestrator/worker web shell、worker followup、harness、設定の操作

## 現在の運用方針

このプロジェクトは、過去の `task-pr-orchestrator` / `task-pr-worker` 運用に合わせて、
Worker が専用 worktree 内で実装、検証、PR 作成、`gh-review-hook`、PR マージまで完了し、
その後 `notify(type="completed")` で PR URL と merge commit を Go ハーネスへ通知する
モデルを採用します。Go ハーネスは worktree / tmux / 台帳 / notify / archive などの機械的な
状態管理を担い、Orchestrator Agent はタスク整理、分割、worker 起動、完了タスクの archive を
MCP ツール経由で判断します。

Orchestrator セッションは Web UI の Orchestrator パネルで、ログ閲覧用の画面ではなく
tmux window に接続された対話可能な Web shell として操作できます。タスク追加、削除、
整理のような継続的な調整は、ブラウザ上の shell に直接入力して選択中プロジェクトの
orchestrator window へ送ります。

## リポジトリ構成

```text
.
├── docs/      要件と実装計画
├── server/    Go バックエンドと ccx serve コマンド
├── tasks/     タスク台帳
└── web/       Vite + React + TypeScript UI
```

## 必要なもの

- Go 1.23 以上
- Node.js と npm
- git
- tmux
- 利用する agent harness の CLI
  - `claude`
  - `codex`
  - `opencode`
  - `cursor-agent`

## セットアップ

```sh
cd server
go test ./...
```

```sh
cd web
npm install
npm run build
```

## 開発時の確認

バックエンドのテスト:

```sh
cd server
go test ./...
go test ./... -race -timeout 120s
```

Web UI の開発サーバー:

```sh
cd web
npm run dev
```

Vite dev server は `127.0.0.1:5173` で起動し、`/api` と `/ws` を
`127.0.0.1:8080` の Go サーバーへ proxy します。

単一プロセス起動:

```sh
cd server
go run ./cmd/ccx
```

既定では `~/.config/ccx-t2/config.yaml` を読みます。存在しない場合は、project を空のままにした
config を自動生成します。利用可能な harness CLI は PATH から検出し、検出できたものだけを
登録します。対応 CLI では `--yolo` や dangerous permissions 系の flag を best-effort で付与します。
Web UI は、配布時には `server/internal/webui/dist` から Go バイナリに埋め込まれたビルド済み
asset を通常のフォールバックとして配信します。起動時には有効な `--web-dir` パス（既定は
`web/dist`）を先に確認し、そのディレクトリが存在する場合だけ外部配信元として優先します。
ローカルの `web/dist` を確認したい開発時の override として使います。

Web UI を更新した場合は、release workflow と同じ流れで `web/src` から `web/dist` をビルドし、
埋め込み用 asset と同期してから差分を検証します。

```sh
cd web
npm run build
npm run embedded:sync
npm run embedded:check
```

`embedded:sync` を実行した場合は、Web UI のソース変更と一緒に
`server/internal/webui/dist` も commit します。
CI では `npm run build` 後に `npm run embedded:check` を実行し、同期漏れを検出します。

## 設定

設定は YAML で扱います。`${VAR}` 形式の環境変数プレースホルダーはロード時に展開されます。
`mcp_secret` や GitHub token のような secret は、Web API ではマスクされます。

例:

```yaml
server:
  host: 127.0.0.1
  port: 8080
  mcp_secret: ${CCX_MCP_SECRET}

runtime:
  tmux_session: ccx-t2
  worktree_base: /path/to/worktrees

orchestrator:
  harness: claude
  heartbeat_interval: 60s
  timeout: 30m

worker_harnesses:
  - claude
  - codex

harnesses:
  claude:
    command: claude
    mcp_args: "--mcp-url {url} --mcp-secret {secret}"
  codex:
    command: codex
    mcp_args: "--mcp-url {url} --mcp-secret {secret}"

projects: {}
```

project は Web UI の Settings から追加できます。

## API

Web UI 向け:

- `GET /api/tasks`
- `POST /api/tasks` — `request` または `body` だけの自然文 intake も登録できます
- `PATCH /api/tasks/{id}`
- `DELETE /api/tasks/{id}`
- `GET /api/workers`
- `POST /api/workers/{worker_id}/followup`
- `GET /api/projects`
- `GET /api/projects/{slug}/tasks`
- `POST /api/projects/{slug}/tasks` — `request` または `body` だけの自然文 intake も登録できます
- `PATCH /api/projects/{slug}/tasks/{id}`
- `DELETE /api/projects/{slug}/tasks/{id}`
- `GET /api/projects/{slug}/workers`
- `POST /api/projects/{slug}/workers/{worker_id}/followup`
- `GET /api/harnesses`
- `GET /api/config`
- `PATCH /api/config`
- `GET /ws/ledger`
- `GET /ws/orchestrator`
- `GET /ws/worker/{window}`
- `GET /ws/projects/{slug}/ledger`
- `GET /ws/projects/{slug}/orchestrator`
- `GET /ws/projects/{slug}/worker/{window}`

MCP 向け:

- `POST /mcp/orchestrator`
- `POST /mcp/worker`

自然文 intake は `Natural language intake` の仮タイトルで台帳に保存され、Orchestrator が
コードベースを調査してから MCP の `update_task` / `create_task` / `split_task` /
`archive_task` で調査結果、実装範囲、禁止範囲、検証方法、または不足情報を明記します。
Web UI の Add Task は自然文入力を `request` として同じ backend API に送ります。

詳細な要件は [docs/requirements.md](docs/requirements.md)、実装の段階計画は
[docs/implementation-plan.md](docs/implementation-plan.md) を参照してください。自然文投入から
PR マージ後の完了通知までの受け入れ確認は
[docs/acceptance-scenario.md](docs/acceptance-scenario.md) にまとめています。

## ライセンス

MIT License。詳細は [LICENSE](LICENSE) を参照してください。
