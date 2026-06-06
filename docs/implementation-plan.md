# 実装計画書

## ディレクトリ構成

```text
ccx-t2/
├── server/                        ← Go バックエンド
│   ├── cmd/
│   │   └── ccx/
│   │       └── main.go           ← エントリーポイント
│   ├── internal/
│   │   ├── config/
│   │   │   └── config.go         ← 設定ファイル読み込み（yaml.v3 で直接 unmarshal）
│   │   ├── ledger/
│   │   │   ├── ledger.go         ← 台帳読み書き（ミューテックスで排他制御）
│   │   │   └── parser.go         ← MD front matter パーサー
│   │   ├── tmux/
│   │   │   └── tmux.go           ← tmux セッション・ウィンドウ管理
│   │   ├── worktree/
│   │   │   └── worktree.go       ← git worktree 管理
│   │   ├── harness/
│   │   │   └── harness.go        ← ハーネス起動・usage 管理
│   │   ├── github/
│   │   │   └── github.go         ← GitHub API クライアント
│   │   ├── orchestrator/
│   │   │   └── orchestrator.go   ← Orchestrator 起動（tmux ウィンドウへのハーネス起動）
│   │   ├── scheduler/
│   │   │   └── scheduler.go      ← ハートビート管理（Orchestrator.Trigger を呼ぶ）
│   │   ├── mcp/
│   │   │   └── server.go         ← MCP HTTP サーバー（/mcp/orchestrator・/mcp/worker）
│   │   ├── event/
│   │   │   └── queue.go          ← Worker notify イベントの直列キュー（chan Event）
│   │   └── web/
│   │       ├── server.go         ← REST API・静的ファイル配信
│   │       └── ws.go             ← WebSocket ハンドラ
│   └── go.mod
├── web/                           ← フロントエンド（Vite + React + TypeScript）
│   ├── src/
│   │   ├── components/
│   │   ├── pages/
│   │   └── main.tsx
│   ├── index.html
│   ├── vite.config.ts
│   ├── tsconfig.json
│   └── package.json
├── tasks/
│   ├── ledger.md                  ← タスク台帳（未着手・進行中）
│   └── archive/                   ← 完了タスク
└── config.yaml
```

---

## フェーズ1：台帳・パーサー

### 目標

`tasks/ledger.md` の読み書きができる状態にする。

### 実装内容

**`internal/ledger/parser.go`**
- パーサーは「body 状態」と「front matter 状態」の2ステートで動作する
- body 状態では、`---` 行を検出したとき次の行が `id:` で始まる場合のみ front matter 開始（front matter 状態へ遷移）とみなし、そうでなければ body 内の水平線として読み飛ばす
- front matter 状態では、`---` 行を検出したとき次の行の内容に関わらず front matter ブロックの終了（body 状態へ遷移）とみなす
- front matter + body のペアをスライスで返す
- front matter は YAML としてパース（`gopkg.in/yaml.v3`）

**`internal/ledger/ledger.go`**
- `Load() ([]Task, error)` — ファイル読み込み・パース
- `Save([]Task) error` — front matter + body を結合してファイル書き込み（tmp ファイル経由のアトミック書き込み）
- `Add(task Task) error`
- `Update(id string, fields map[string]any) error`
- `Archive(id string) error` — `tasks/archive/` にファイルを切り出し
- 全メソッドで `sync.Mutex` による排他制御（REST・MCP の両経路からの書き込みに対応）

**`internal/ledger/ledger_test.go`**
- パーサーのユニットテスト
- 複数タスクの読み書きラウンドトリップテスト

---

## フェーズ2：tmux・Worktree 管理

### 目標

tmux ウィンドウの作成・削除・キー送信と git worktree の管理ができる状態にする。

### 実装内容

**`internal/tmux/tmux.go`**
- `EnsureSession(slug string) error` — セッションがなければ作成
- `CreateWindow(session, name string) error`
- `SendKeys(session, window, keys string) error`
- `KillWindow(session, window string) error`
- `PipeOutput(session, window string) (<-chan string, error)` — ログストリーミング用

**`internal/worktree/worktree.go`**
- `Create(repoPath, branch, worktreePath string) error` — `git worktree add -b {branch}`（ブランチを新規作成）
- `Remove(worktreePath string) error` — `git worktree remove`

---

## フェーズ3：MCP サーバー

**フェーズ2の完了が前提。** `spawn_worker`・`stop_worker` はフェーズ2の tmux・worktree 実装に依存する。

### 目標

Orchestrator・Worker がツールを呼べる HTTP MCP サーバーを立てる。

### 実装内容

**`internal/mcp/server.go`**
- HTTP サーバー（MCP over Streamable HTTP）
- ツール定義の登録とルーティング
- JSON-RPC 形式のリクエスト/レスポンス処理
- エンドポイントでロールを分離し、Orchestrator 向けと Worker 向けを別パスで提供

**エンドポイントによるロール分離：**
- `POST /mcp/orchestrator` — Orchestrator 向けツール（10本）のみ受け付ける
- `POST /mcp/worker` — Worker 向けツール（notify のみ）のみ受け付ける
- 各エンドポイントは登録外のツール名を受け取った場合にエラーを返し、ロールを越えた呼び出しを拒否する

実装するツール（要件定義書の MCP セクション参照）：
- Orchestrator 向け（`/mcp/orchestrator`）: list_tasks, create_task, update_task, split_task, archive_task, list_harnesses, spawn_worker, stop_worker, followup_worker, get_pr_status
- Worker 向け（`/mcp/worker`）: notify

`spawn_worker` の実行順序：
1. `worktree.Create(repoPath, branch, worktreePath)` — worktree を生成してブランチを作成
2. `tmux.CreateWindow(session, "worker-{task_id}")` — tmux ウィンドウを作成
3. ハーネス起動コマンドを組み立て（worktree path・MCP URL `/mcp/worker`・タスク詳細を注入）
4. `tmux.SendKeys` でハーネスを起動
5. 台帳の status を in_progress に更新し branch / worker_id / harness フィールドを設定

ステップ5（台帳更新）が失敗した場合、ウィンドウを `KillWindow` で停止した後に worktree を削除してエラーを返す。ステップ1〜4が失敗した場合、それまでに作成済みのリソース（worktree・ウィンドウ）を逆順で削除してエラーを返す。

---

## フェーズ4：イベントキュー・Orchestrator トリガー

### 目標

Worker からのイベントを直列に処理し、Orchestrator を適切なタイミングで起動する。

### 実装内容

**`internal/event/queue.go`**
- Worker notify イベントを受け取る FIFO キュー（バッファ付き `chan Event`）
- goroutine で1件ずつ処理（直列）
- イベント種別に応じて台帳を更新してから Orchestrator.Trigger を呼ぶ
  - `completed`：status を `completed` に設定し payload の `pr_url` を台帳に書き込む（`merge_commit` はアーカイブファイルに記録するため台帳には書かない）
  - `blocked`：status を `blocked` に設定し、payload の `reason` を台帳に書き込む
  - `split_request`：元タスクの status を `split` に設定し、payload の `proposed_slices` を `unstarted` 子タスクとして台帳に追加する

**`internal/orchestrator/orchestrator.go`**
- `Trigger(ctx context.Context, reason string) error`
  - 台帳スナップショット・ハーネス一覧をプロンプトに注入
  - 設定されたハーネスで Orchestrator を tmux の `orchestrator` ウィンドウで起動
  - 既存の Orchestrator が実行中の場合はキューイングし、完了後に処理
- Orchestrator ウィンドウの実行状態は tmux ウィンドウの存在と pane の状態で判定する

**`internal/scheduler/scheduler.go`**
- ハートビートタイマー（`time.Ticker`）
- 台帳に unstarted / in_progress / blocked タスクがある間は定期的に `Trigger` を呼ぶ
- 台帳に unstarted / in_progress / blocked のタスクが1件もなくなったらタイマーを停止（split タスクは台帳に残るが停止条件の対象外）

---

## フェーズ5：Web UI

### 目標

台帳 CRUD・Worker ログストリーミング・タスク追加フォームが動く Web UI を実装する。

### 実装内容

**`server/internal/web/server.go`**
- REST API
  - `GET /api/tasks` — 台帳一覧
  - `POST /api/tasks` — タスク追加（Orchestrator にトリガー）
  - `PATCH /api/tasks/:id` — タスク更新
  - `DELETE /api/tasks/:id` — タスク削除（in_progress の場合は stop_worker・worktree 削除を連動）
  - `GET /api/workers` — Worker 一覧（内部 Worker レジストリから返す。tmux ウィンドウ一覧と同期）
  - `GET /api/harnesses` — ハーネス一覧
  - `GET /api/config` — 設定取得（GitHub トークン等のシークレットは返さない）
  - `PATCH /api/config` — 設定更新
- 本番ビルド時は `web/dist/` をビルド済み静的ファイルとして配信

**`server/internal/web/ws.go`**
- `GET /ws/worker/:window` — tmux pipe-pane をブリッジして Worker ログをストリーミング
- `GET /ws/ledger` — 台帳の変更（MCP ツール実行・REST 書き込みの後）を push 通知

**`web/` （Vite + React + TypeScript）**
- 開発時は Vite dev server（デフォルト `localhost:5173`）を起動し、`/api` と `/ws` を Go サーバー（デフォルト `localhost:8080`）にプロキシする
- 本番ビルド（`vite build`）の出力 `web/dist/` を Go サーバーが配信する
- 画面構成
  - タスク台帳ビュー（一覧・編集・削除）
  - タスク追加フォーム（自然言語入力 → Orchestrator 経由）
  - Worker ダッシュボード（ログストリーミング）
  - 設定画面

---

## フェーズ6：ハーネス統合テスト

### 目標

各ハーネスで Worker が正常に動作することを確認する。

### 確認内容
- Claude Code での Worker 起動・notify・完了フロー
- Codex / OpenCode での同フロー
- Cursor Agent は CLI 提供状況を確認した上で対応
- Orchestrator のハーネス選択（難易度・usage 残量による分岐）
- タスク分割フロー（split_task → 子タスク Worker 起動）

---

## 依存ライブラリ候補

| 用途 | ライブラリ |
|---|---|
| YAML パース（設定・台帳） | `gopkg.in/yaml.v3` |
| WebSocket | `github.com/gorilla/websocket` |
| GitHub API | `github.com/google/go-github/v60` |

設定ファイルは `yaml.v3` で struct に直接 unmarshal する。CLI フレームワーク・設定管理ライブラリ（cobra, viper）は使用しない。

---

## 実装順序まとめ

| フェーズ | 内容 | 前提 | 完了条件 |
|---|---|---|---|
| 1 | 台帳・パーサー | なし | ledger.md の CRUD が動くユニットテストが通る |
| 2 | tmux・Worktree | なし | tmux ウィンドウの作成・削除・ログ取得ができる |
| 3 | MCP サーバー | フェーズ2 | curl でツールを呼べる |
| 4 | イベントキュー・Orchestrator トリガー | フェーズ3 | Worker notify → Orchestrator 起動が動く |
| 5 | Web UI | フェーズ4 | ブラウザから台帳操作・Worker ログ閲覧ができる |
| 6 | ハーネス統合テスト | フェーズ5 | 対応ハーネスでエンドツーエンドフローが通る |
