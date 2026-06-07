# ccx-t2

`ccx-t2` は、`task-pr-orchestrator` / `task-pr-worker` の運用を補助するための
Go バックエンドと React Web UI です。タスク台帳、worker の tmux セッション、
worktree、MCP エンドポイント、PR 状態を一つのローカルハーネスとして扱います。

## 主な機能

- `tasks/ledger.md` 形式のタスク台帳の読み書き、作成、更新、分割、削除
- task ごとの git worktree と tmux worker window の管理
- orchestrator / worker 向け MCP HTTP エンドポイント
- worker 完了通知、フォローアップ、PR 状態確認
- WebSocket による台帳と worker ログの更新通知
- React Web UI によるタスク、worker、harness、設定の操作

## リポジトリ構成

```text
.
├── docs/      要件と実装計画
├── server/    Go バックエンドの internal packages
├── tasks/     タスク台帳
└── web/       Vite + React + TypeScript UI
```

現時点では Go サーバーの `cmd` / `main` エントリポイントは含まれていません。
バックエンドの機能は `server/internal/...` に実装され、Go テストで検証されています。

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

## 設定

設定は YAML で扱います。`${VAR}` 形式の環境変数プレースホルダーはロード時に展開されます。
`mcp_secret` や GitHub token のような secret は、Web API ではマスクされます。

例:

```yaml
project:
  slug: my-project
  repo_path: /path/to/repo
  worktree_base: /path/to/worktrees
  validation_command: go test ./...

server:
  port: 8080
  mcp_secret: ${CCX_MCP_SECRET}

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

github:
  token: ${GITHUB_TOKEN}
  owner: org
  repo: repo-name
```

## API

Web UI 向け:

- `GET /api/tasks`
- `POST /api/tasks`
- `PATCH /api/tasks/{id}`
- `DELETE /api/tasks/{id}`
- `GET /api/workers`
- `GET /api/harnesses`
- `GET /api/config`
- `PATCH /api/config`
- `GET /ws/ledger`
- `GET /ws/worker/{window}`

MCP 向け:

- `POST /mcp/orchestrator`
- `POST /mcp/worker`

詳細な要件は [docs/requirements.md](docs/requirements.md)、実装の段階計画は
[docs/implementation-plan.md](docs/implementation-plan.md) を参照してください。

## ライセンス

MIT License。詳細は [LICENSE](LICENSE) を参照してください。
