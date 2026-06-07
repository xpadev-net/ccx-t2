# 要件定義書

## 概要

task-pr-orchestrator / task-pr-worker スキルのフローを複数のエージェントハーネスを跨いで実行できるようにするハーネスを Go + Web UI で実装する。オーケストレーションのうち機械的に処理できる部分（ハートビート、イベントルーティング、プロセス管理、台帳永続化）を Go 側に寄せ、AI には判断のみを担わせる。

ハーネスは単一プロセスで起動し、グローバル設定ファイルに登録された複数プロジェクトを同時に扱う。プロジェクトごとの `config.yaml` は作成しない。Web UI、REST API、MCP サーバー、WebSocket、scheduler は同じプロセス内で動作し、リクエストまたはイベントに含まれる `project_slug` で対象プロジェクトを解決する。

---

## 役割分担

### Go ハーネスが担う

- タスク台帳の永続化・読み書き
- Worktree の生成・削除
- Worker プロセス（tmux ウィンドウ）の起動・停止
- Worker へのフォローアップメッセージ送信（tmux send-keys）
- Worker からのイベント受信・直列キュー処理
- Orchestrator の起動トリガー（ハートビート・イベント駆動）
- GitHub API による PR 状態取得
- Web UI へのリアルタイム配信（WebSocket）
- MCP サーバー（HTTP）の提供
- グローバル設定ファイルの読み書き
- 複数プロジェクトの runtime（台帳、worker registry、scheduler、orchestrator）管理

### Orchestrator Agent が担う

- どのタスクを Worker にアサインするか（MCP ツールを呼んで Go 側に命令を発行）
- コンフリクト判定（ファイル所有権）
- タスク分割の粒度・ブランチ設計
- タスク難易度と各ハーネスの usage 残量を見てハーネスを選択
- コードベース調査を経たタスク追加・台帳反映
- 構造化 JSON を返すのではなく MCP ツールコールで命令を発行するエージェント型動作

### Worker Agent が担う

- 委任されたタスクの実装
- バリデーション・テスト
- セルフレビュー
- PR 作成・マージ
- 完了・ブロック・分割要求の通知

---

## タスク台帳

### フォーマット

各プロジェクトの `tasks/ledger.md` に未着手・進行中タスクをまとめて管理する。台帳パスは `repo_path/tasks/ledger.md` を既定値とし、必要な場合はプロジェクト設定で明示的に上書きできる。各タスクは `---` で開始・終了する front matter ブロック + markdown body のセットで記述する。

**パーサー規約：** `---` 行が front matter の区切りか markdown 水平線かは、その次の行で判定する。`---` の直後の行が `id:` で始まる場合のみ front matter 開始とみなし、そうでなければ body 内の水平線として扱う。`id` は全タスクの front matter で必ず第1フィールドとして記述する。この規約により body 内で自由に `---` を使用できる。ただし、body 内で `---` の直後に `id:` で始まる行（YAML コード例など）を記述した場合は誤ってタスク境界と解釈されるため、body 内でこのパターンは使用しないこと。

```markdown
---
id: task-001
title: Add user authentication endpoint
status: unstarted
allowed_files: [src/auth/, src/middleware/auth.go]
forbidden_files: [go.sum]
---

JWTベースの認証エンドポイントを追加する。

---
id: task-002
title: User profile page
status: in_progress
branch: feature/user-profile
worker_id: worker-task-002
harness: claude
pr_url: https://github.com/org/repo/pull/42
updated_at: 2026-06-04T12:30:00Z
---

ユーザープロフィールページの実装。

### ブロッカー

（なし）
```

### ステータス遷移

```text
unstarted → in_progress → completed → (archive_task でアーカイブに移動・台帳から削除)
                        → blocked
                        → split   ← 終端。子タスクが unstarted で追加される。再スケジュールしない。
```

`completed` は Worker の `notify(completed)` を受けて Go が設定する遷移ステータスであり、`archive_task` が呼ばれると同時に台帳から削除される。`split` は終端ステータスであり、ハートビートのスケジューリング対象外とする。

ハートビートの「未完了タスクが存在する間」は `unstarted` または `in_progress` または `blocked` のタスクが1件以上ある状態を指す。

### アーカイブ

完了タスクは `tasks/archive/task-{id}-{slug}.md` に切り出す。台帳は常にアクティブなタスクのみ保持する。

### front matter フィールド

| フィールド | 説明 |
|---|---|
| id | タスク一意ID |
| title | タスク名 |
| status | unstarted / in_progress / blocked / split / completed |
| branch | Gitブランチ名（spawn_worker 時に Go が設定） |
| worker_id | tmuxウィンドウ名（spawn_worker 時に Go が設定） |
| harness | 使用するハーネス名（spawn_worker 時に Go が設定） |
| allowed_files | 編集を許可するパス |
| forbidden_files | 編集を禁止するパス |
| pr_url | PR URL（Worker が作成後に notify payload 経由で設定） |
| merge_commit | マージコミットハッシュ（完了後にアーカイブファイルに記録） |
| updated_at | 最終更新日時 |

---

## tmux 構造

```text
session: ccx-t2
  window: {project-slug}-orchestrator       ← Orchestrator Agent（トリガーのたびに同ウィンドウで再起動）
  window: {project-slug}-worker-{task-id}  ← Worker Agent（タスクごとに1ウィンドウ）
  window: {project-slug}-worker-{task-id}
  ...
```

単一プロセスにつき1 tmux セッションを既定とし、window 名に `project_slug` を含めてプロジェクト間の衝突を避ける。Worker はタスクへのアサインと同時に作成し、完了・停止時に削除する。

Orchestrator ウィンドウはプロジェクトごとに1つのみ維持する。トリガー時に同一プロジェクトの既存ウィンドウが実行中の場合はキューイングし、完了後に次のトリガーを処理する。別プロジェクトの Orchestrator / Worker は同一プロセス内で独立して動作できる。

---

## 起動

単一コマンドでプロセス全体を起動する。

```sh
ccx
```

`serve` は以下をまとめて起動する。

- REST API / WebSocket / MCP HTTP サーバー
- ビルド済み Web UI の静的ファイル配信
- グローバル設定に登録された各プロジェクトの ledger runtime
- 各プロジェクトの heartbeat scheduler
- 共通 tmux session

`ccx serve` も互換のため同じ動作をする。`--config` が省略された場合は
`~/.config/ccx-t2/config.yaml` を使用する。設定ファイルが存在しない場合は、
project を空のままにした設定ファイルを自動生成する。PATH 上に存在する harness CLI は
自動検出し、検出できたものだけを `harnesses` / `worker_harnesses` に追加する。
対応 CLI では `--yolo` や dangerous permissions 系の flag を best-effort で付与し、
権限確認で worker が止まらないようにする。

Web UI はビルド済み asset を Go バイナリに埋め込み、既定ではその asset を配信する。
開発時に外部 `dist` を確認したい場合のみ `--web-dir` で配信ディレクトリを上書きできる。

---

## 対応ハーネス

Orchestrator・Worker ともに以下から選択可能とする。

- Claude Code（`claude`）
- Codex（`codex`）
- OpenCode（`opencode`）
- Cursor Agent（`cursor-agent`）— 非対話型 CLI の安定提供状況に依存。利用可否は統合テストで確認する。

---

## MCP サーバー（HTTP）

エンドポイントでロールを分離し、対象プロジェクトはリクエスト payload の `project_slug` で指定する。

- `POST /mcp/orchestrator` — Orchestrator 向けツールのみ受け付ける
- `POST /mcp/worker` — Worker 向けツール（notify のみ）のみ受け付ける

各エンドポイントは登録外のツール名を受け取った場合にエラーを返す。Worker 起動時に注入する MCP URL は `/mcp/worker` を指定し、プロンプトにも `project_slug` を注入する。`project_slug` が未登録の場合はエラーを返し、どの台帳・worktree・tmux window も変更しない。

### Orchestrator 向けツール

```text
list_projects()
  → 登録済みプロジェクト一覧

list_tasks(project_slug)
  → 台帳のスナップショット（全アクティブタスク）

create_task(project_slug, title, description)
  → 台帳に unstarted タスクを追加

update_task(project_slug, id, fields{})
  → 台帳の front matter フィールドを更新

split_task(project_slug, id, reason, slices[{title, description, allowed_files, forbidden_files}])
  → 元タスクを split に更新し、子タスクを unstarted で追加

archive_task(project_slug, id)
  → タスクを台帳から削除してアーカイブに移動

list_harnesses()
  → 利用可能ハーネスの一覧。usage 残量はハーネスごとに取得方法が異なるため
    近似値または設定値を返す場合がある。

spawn_worker(project_slug, task_id, branch, allowed_files, forbidden_files, harness)
  → ブランチを作成（git worktree add -b）し、tmux ウィンドウを作成してハーネスを起動。
    台帳の status を in_progress に更新し、branch / worker_id / harness フィールドを設定する。
    ハーネスバイナリが見つからない場合はエラーを返し台帳を更新しない。

stop_worker(project_slug, worker_id)
  → tmux ウィンドウを終了し、対応する worktree を削除する。

followup_worker(project_slug, worker_id, message)
  → tmux send-keys でメッセージを送信。

get_pr_status(project_slug, pr_number)
  → GitHub API で PR の現在状態を返す
```

### Worker 向けツール

```text
notify(type, payload)
  type: "completed" | "blocked" | "split_request"
  payload: {
    project_slug,
    task_id,
    pr_url?,         -- completed 時
    merge_commit?,   -- completed 時
    reason?,         -- blocked / split_request 時
    proposed_slices? -- split_request 時
  }
```

---

## Orchestrator のトリガー

| 契機 | 動作 |
|---|---|
| ハートビート（定期） | 台帳に unstarted / in_progress / blocked タスクが存在する間、定期的に起動 |
| Worker の notify | 受信後即時・直列キューで処理、Orchestrator を起動 |
| Web UI からタスク追加 | 対象プロジェクトとユーザー入力を渡して即時起動（コードベース調査 → 台帳反映） |

Orchestrator 起動時は対象プロジェクトの設定、台帳スナップショット、ハーネス一覧をプロンプトに注入する。Orchestrator はステートレスで、起動のたびに現在状態を読んで判断する。

台帳が大きい場合はプロンプトがコンテキスト上限を超える可能性がある。タスクの description は簡潔に保つことを推奨する。

---

## Worker の起動フロー

1. Orchestrator が `spawn_worker()` を呼ぶ
2. Go が `project_slug` から対象プロジェクトを解決する
3. Go が対象リポジトリでブランチを作成し worktree を生成する
4. Go が tmux ウィンドウを作成し、タスク詳細（project_slug, id, title, branch, allowed_files, forbidden_files, worktree path, 検証コマンド, MCP サーバー URL）をプロンプトまたはハーネス固有の設定フラグで注入してハーネスを起動する
5. Worker はプロンプトに従って実装・レビュー・PR 作成・マージを実行する
6. 完了・ブロック・分割要求の際に `notify()` を呼ぶ
7. Go がイベントを受信し、同じプロジェクトの Orchestrator を即時起動する

---

## Web UI

### 機能

| 機能 | 説明 |
|---|---|
| タスク台帳 CRUD | タスクの追加・編集・削除。削除は in_progress タスクに対して stop_worker と worktree 削除を連動して実行する。追加は Orchestrator 経由。 |
| Worker ダッシュボード | 各 Worker（tmux ウィンドウ）のログをリアルタイムストリーミング |
| タスク追加フォーム | 自然言語でタスクを入力 → Orchestrator がコードベースを調査して台帳に反映 |
| ハーネス設定 | 利用可能ハーネスの登録・usage 確認 |
| プロジェクト選択 | グローバル設定に登録されたプロジェクトを切り替え、選択中プロジェクトのタスクと worker を表示 |
| Orchestrator 設定 | グローバル既定値とプロジェクトごとの上書き設定（使用するハーネス・ハートビート間隔等） |

### 技術

- フロントエンド：Web（詳細は実装フェーズで決定）
- バックエンド：Go
- リアルタイム通信：WebSocket（Worker ログ配信・台帳更新通知）
- REST と MCP の両方が台帳を書き換えるため、Go プロセス内でミューテックスによる排他制御を行う

---

## 設定ファイル

グローバル設定ファイルのみを使用する。プロジェクトごとの `config.yaml` は作成しない。

既定パス:

```text
~/.config/ccx-t2/config.yaml
```

```yaml
server:
  port: 8080
  mcp_secret: ${CCX_MCP_SECRET}

runtime:
  tmux_session: ccx-t2
  worktree_base: ~/.local/share/ccx-t2/worktrees

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
  opencode:
    command: opencode
    mcp_args: "--mcp-url {url} --mcp-secret {secret}"
  cursor-agent:
    command: cursor-agent
    mcp_args: "--mcp-url {url} --mcp-secret {secret}"

projects: {}
```
