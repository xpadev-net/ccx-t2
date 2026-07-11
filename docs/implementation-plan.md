# 実装計画書

## ディレクトリ構成

```text
ccx-t2/
├── server/                        ← Go バックエンド
│   ├── cmd/
│   │   └── ccx/
│   │       └── main.go           ← `ccx serve` エントリーポイント
│   ├── internal/
│   │   ├── config/
│   │   │   └── config.go         ← グローバル設定ファイル読み込み（yaml.v3 で直接 unmarshal）
│   │   ├── runtime/
│   │   │   └── manager.go        ← 複数プロジェクト runtime の生成・参照
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
│   │   ├── web/
│   │   │   ├── server.go         ← REST API
│   │   │   └── ws.go             ← WebSocket ハンドラ
│   │   └── webui/
│   │       ├── webui.go          ← embed された Web UI asset の http.Handler
│   │       └── dist/             ← Go バイナリへ埋め込むビルド済み Web UI
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
└── tasks/
    ├── ledger.md                  ← このリポジトリ自身のタスク台帳
    └── archive/                   ← 完了タスク
```

---

## 前提アーキテクチャ

- 起動は `ccx` の単一コマンドに集約する。`ccx serve` も互換 alias として受け付ける
- `--config` 省略時は `~/.config/ccx-t2/config.yaml` を読む
- 設定ファイルが存在しない場合は、projects を空にした設定ファイルを自動生成する
- 初回生成時は PATH 上の対応 harness CLI を検出し、存在するものだけを `harnesses` / `worker_harnesses` に追加する
- 検出した harness の起動引数には、`--yolo` や dangerous permissions 系の flag を best-effort で含める
- Web UI のビルド済み asset は `server/internal/webui/dist` から Go バイナリに embed し、配布時の通常フォールバックとする。有効な `--web-dir` パス（既定は `web/dist`）が存在する場合だけ外部 directory を優先する
- 設定ファイルはグローバルに1つだけ持ち、`projects` 配下に複数プロジェクトを登録する
- Go プロセスは REST API / WebSocket / MCP / Web UI 静的配信 / heartbeat scheduler をまとめて起動する
- 各プロジェクトは `project_slug` をキーに、ledger、worker registry、orchestrator、scheduler を持つ
- tmux session はプロセスで1つを既定とし、window 名は `{project_slug}-orchestrator`、`{project_slug}-worker-{task_id}` とする
- API / MCP / WebSocket は `project_slug` を受け取り、対象プロジェクトの runtime にルーティングする

---

## フェーズ1：台帳・パーサー

### 目標

プロジェクトごとに指定された `ledger_path` の読み書きができる状態にする。

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
- ledger のパスは constructor で受け取り、runtime manager がプロジェクト設定から生成する

**`internal/ledger/ledger_test.go`**
- パーサーのユニットテスト
- 複数タスクの読み書きラウンドトリップテスト

**`internal/config/config.go`**
- `Config` は `server` / `runtime` / `orchestrator` / `worker_harnesses` / `harnesses` / `projects` を持つグローバル設定とする
- `ProjectConfig` は `repo_path` / `ledger_path` / `validation_command` / `github` / `orchestrator` override を持つ
- `runtime.worktree_base` と `runtime.tmux_session` はグローバル既定値とする
- `ledger_path` 未指定時は `{repo_path}/tasks/ledger.md` を使う
- `${VAR}` 形式の環境変数展開と validation をグローバル設定全体に適用する

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
- `Remove(repoPath, worktreePath string) error` — `git -C {repoPath} worktree remove --force`
- `DeleteTaskBranchIfSafe(repoPath, branch, taskID string) error` — task ID に明確に紐づく cleanup 用の local branch、または移行用の namespaced legacy branch のみ削除し、default branch、別 worktree で checkout 中のブランチ、remote/upstream ref を持つブランチは削除しない

worktree path は `runtime.worktree_base/{project_slug}-{task_id}` を既定とする。

---

## フェーズ2.5：単一コマンド起動・runtime manager

### 目標

`ccx serve` でグローバル設定を読み、複数プロジェクトの runtime を単一プロセス内に構築する。

### 実装内容

**`server/cmd/ccx/main.go`**
- `ccx [serve] --config <path>` を実装する
- `--config` 省略時は `~/.config/ccx-t2/config.yaml`
- 設定ファイルが存在しない場合は、projects 空・利用可能 harness 自動検出済みの既定設定を生成して保存する
- 設定をロードし、tmux session を ensure する
- `runtime.Manager` を生成し、各プロジェクトの ledger / registry / orchestrator / scheduler を初期化する
- HTTP server を `server.host` / `server.port` で起動する
- 有効な `--web-dir` パス（既定は `web/dist`）が存在する場合は外部 directory を静的配信し、存在しない場合は `server/internal/webui/dist` から embed された Web UI asset を配信する
- SIGINT / SIGTERM で HTTP server と scheduler を graceful shutdown する

**`internal/runtime/manager.go`**
- `ProjectRuntime` に `Slug`, `Config`, `Ledger`, `Registry`, `Orchestrator`, `Scheduler` を保持する
- `Manager.Project(slug)` で登録済みプロジェクトを返す
- `Manager.Projects()` で Web UI / MCP 用にプロジェクト一覧を返す
- `Manager.Start(ctx)` で全プロジェクトの scheduler を起動する
- 未登録 `project_slug` は typed error にして REST / MCP で 404 / tool error に変換する

---

## フェーズ3：MCP サーバー

**フェーズ2.5の完了が前提。** `spawn_worker`・`stop_worker` はフェーズ2の tmux・worktree 実装とフェーズ2.5の runtime manager に依存する。

### 目標

Orchestrator・Worker がツールを呼べる HTTP MCP サーバーを立てる。

### 実装内容

**`internal/mcp/server.go`**
- HTTP サーバー（MCP over Streamable HTTP）
- ツール定義の登録とルーティング
- JSON-RPC 形式のリクエスト/レスポンス処理
- エンドポイントでロールを分離し、Orchestrator 向けと Worker 向けを別パスで提供

**エンドポイントによるロール分離：**
- `POST /mcp/orchestrator` — Orchestrator 向けツール（11本）のみ受け付ける
- `POST /mcp/worker` — Worker 向けツール（notify のみ）のみ受け付ける
- 各エンドポイントは登録外のツール名を受け取った場合にエラーを返し、ロールを越えた呼び出しを拒否する

実装するツール（要件定義書の MCP セクション参照）：
- Orchestrator 向け（`/mcp/orchestrator`）: list_projects, list_tasks, create_task, update_task, split_task, archive_task, list_harnesses, spawn_worker, stop_worker, followup_worker, get_pr_status
- Worker 向け（`/mcp/worker`）: notify

`spawn_worker` の実行順序：
1. `project_slug` で runtime manager から対象プロジェクトを解決
2. `branch` が `task_id` を delimiter-bounded segment として含むことを検証し、`worktree.Create(project.repo_path, branch, worktreePath)` で worktree を生成してブランチを作成
3. `tmux.CreateWindow(runtime.tmux_session, "{project_slug}-worker-{task_id}")` — tmux ウィンドウを作成
4. ハーネス起動コマンドを組み立て（project_slug・worktree path・MCP URL `/mcp/worker`・タスク詳細を注入）
5. `tmux.SendKeys` でハーネスを起動
6. 対象プロジェクトの台帳の status を in_progress に更新し branch / worker_id / harness フィールドを設定

ステップ6（台帳更新）が失敗した場合、ウィンドウを `KillWindow` で停止した後に worktree を削除してエラーを返す。ステップ1〜5が失敗した場合、それまでに作成済みのリソース（worktree・ウィンドウ）を逆順で削除してエラーを返す。
rollback / stop / archive / split / task delete cleanup で branch を削除する場合は `DeleteTaskBranchIfSafe` を経由し、default branch や PR が存在し得る remote/upstream 付き branch の履歴を壊さない。

---

## フェーズ4：イベントキュー・Orchestrator トリガー

### 目標

Worker からのイベントを直列に処理し、Orchestrator を適切なタイミングで起動する。

### 実装内容

**`internal/event/queue.go`**
- Worker notify イベントを受け取る FIFO キュー（バッファ付き `chan Event`）
- goroutine で1件ずつ処理（直列）
- Event は `project_slug` を必須とし、runtime manager から対象プロジェクトの台帳と orchestrator を解決する
- イベント種別に応じて台帳を更新してから Orchestrator.Trigger を呼ぶ
  - `completed`：status を `completed` に設定し payload の `pr_url` を台帳に書き込む（`merge_commit` はアーカイブファイルに記録するため台帳には書かない）
  - `blocked`：status を `blocked` に設定し、payload の `reason` を台帳に書き込む
  - `split_request`：元タスクの status を `split` に設定し、payload の `proposed_slices` を `unstarted` 子タスクとして台帳に追加する

**`internal/orchestrator/orchestrator.go`**
- `Trigger(ctx context.Context, reason string) error`
  - project_slug・対象プロジェクト設定・台帳スナップショット・ハーネス一覧をプロンプトに注入
  - 設定されたハーネスで Orchestrator を tmux の `{project_slug}-orchestrator` ウィンドウで起動
  - 同一プロジェクトの既存 Orchestrator が実行中の場合はキューイングし、完了後に処理
- Orchestrator ウィンドウの実行状態は tmux ウィンドウの存在と pane の状態で判定する

**`internal/scheduler/scheduler.go`**
- ハートビートタイマー（`time.Ticker`）
- 台帳に unstarted / in_progress / blocked タスクがある間は定期的に `Trigger` を呼ぶ
- 台帳に unstarted / in_progress / blocked のタスクが1件もなくなったらタイマーを停止（split タスクは台帳に残るが停止条件の対象外）

---

## フェーズ5：Web UI

### 目標

project sidebar、project-scoped terminal tabs、1つの full-height xterm からなる WebShell を実装する。
task/orchestrator/worker dashboard は primary UI から外し、harness window は protected terminal entry として統合する。

### 実装内容

**`server/internal/web/server.go`**
- REST API
  - `GET /api/projects` — 登録済みプロジェクト一覧
  - `GET /api/projects/:slug/tasks` — 対象プロジェクトの台帳一覧
  - `POST /api/projects/:slug/tasks` — タスク追加（対象プロジェクトの Orchestrator にトリガー）
    - `request` または `body` のみの自然文 intake を許可し、仮タイトル `Natural language intake` で台帳へ保存する
    - Orchestrator プロンプトは、allowed_files なしの自然文 intake を worker-ready とみなさず、調査後に update/create/split/archive 系 MCP ツールで調査結果・実装範囲・禁止範囲・検証方法またはブロッカーを台帳へ残すよう指示する
  - `PATCH /api/projects/:slug/tasks/:id` — タスク更新
  - `DELETE /api/projects/:slug/tasks/:id` — タスク削除（in_progress の場合は stop_worker・worktree 削除を連動）
  - `GET /api/projects/:slug/workers` — 対象プロジェクトの Worker 一覧（内部 Worker レジストリから返す。tmux ウィンドウ一覧と同期）
  - `POST /api/projects/:slug/workers/:worker_id/followup` — 対象プロジェクトの active Worker tmux window に followup 入力を送信
  - `GET /api/projects/:slug/terminals` — project window を stable DTO で列挙
  - `POST /api/projects/:slug/terminals` — repository cwd の project shell を作成
  - `DELETE /api/projects/:slug/terminals/:window` — closable shell だけを削除し harness window を保護
  - `GET /api/harnesses` — ハーネス一覧
  - `GET /api/config` — グローバル設定取得（GitHub トークン等のシークレットは返さない）
  - `PATCH /api/config` — グローバル設定更新。プロジェクト追加・更新・削除を含む
- 静的ファイル配信は `server/cmd/ccx/main.go` で登録される。有効な `--web-dir` パス（既定は `web/dist`）が存在する場合は外部 directory を配信し、存在しない場合は `server/internal/webui/dist` から embed された asset を配信する

**`server/internal/web/ws.go`**
- `GET /ws/projects/:slug/terminal/:window` — snapshot/live 境界を保った generic terminal stream。raw input、resize、keepalive、slow-client resync を扱う
- `GET /ws/projects/:slug/orchestrator` — tmux pipe-pane をブリッジして Orchestrator ログをストリーミング
- `GET /ws/projects/:slug/worker/:window` — tmux pipe-pane をブリッジして Worker ログをストリーミング
- `GET /ws/projects/:slug/ledger` — 対象プロジェクトの台帳変更（MCP ツール実行・REST 書き込みの後）を push 通知

**`web/` （Vite + React + TypeScript）**
- 開発時は Vite dev server（デフォルト `localhost:5173`）を起動し、`/api` と `/ws` を Go サーバー（デフォルト `localhost:8080`）にプロキシする
- 本番ビルド（`vite build`）の出力 `web/dist/` は `npm run embedded:sync` で `server/internal/webui/dist` に同期し、Go サーバーは同期済みの embed asset を通常配信する
- release workflow は `cd web && npm run build && npm run embedded:sync && npm run embedded:check` で `web/src`、`web/dist`、`server/internal/webui/dist` を同期・検証し、同期後の `server/internal/webui/dist` を Web UI のソース変更と一緒に commit する。CI は `npm run build` 後に `npm run embedded:check` を実行し、同期漏れを stale asset として検出する
- 画面構成
  - project と terminal を階層表示する sidebar
  - 選択 project の horizontal tab strip
  - 選択 terminal 1つだけを表示する full-height xterm surface
  - New shell、closable shell の確認付き close、refresh、manual reconnect
  - connecting/open/retrying/missing/unauthorized/failed の接続状態
  - orchestrator / worker を close 不可の protected terminal として表示
- lifecycle / recovery
  - project+window generation で stale socket/output/input/resize を拒否
  - bounded exponential delay + jitter で無期限 retry
  - online / visibility / manual retry で待機中接続をwake
  - reconnect 後に snapshot/live stream と最後の resize を復元

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
| 2.5 | 単一コマンド起動・runtime manager | フェーズ1,2 | `ccx serve` が複数プロジェクト runtime と HTTP server を起動できる |
| 3 | MCP サーバー | フェーズ2.5 | curl で project_slug 付きツールを呼べる |
| 4 | イベントキュー・Orchestrator トリガー | フェーズ3 | Worker notify → Orchestrator 起動が動く |
| 5 | WebShell UI | フェーズ4 | projectごとのshellを作成・切替・入力・resize・再接続・closeでき、harness windowが保護される |
| 6 | ハーネス統合テスト | フェーズ5 | 対応ハーネスでエンドツーエンドフローが通る |
