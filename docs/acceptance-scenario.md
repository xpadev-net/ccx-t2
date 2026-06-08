# 受け入れシナリオ: 自然言語投入から実装完了まで

この文書は、自然文タスク投入から PR マージ後の完了通知までのユーザーストーリー全体を確認するための受け入れテスト計画である。自動テストで再現する範囲と、GitHub・実ハーネス・ブラウザ・tmux が必要な手動確認範囲を分ける。

対象ストーリー:

1. ユーザーが自然文でタスクを追加する。
2. Go ハーネスが台帳へ `Natural language intake` として保存し、Orchestrator を起動する。
3. Orchestrator がコードベース調査後に worker-ready なタスクへ整理し、`spawn_worker` を呼ぶ。
4. Go ハーネスが専用 worktree と tmux worker window を作成し、Worker を起動する。
5. Worker が専用 worktree 内で実装、検証、PR 作成、`gh-review-hook`、マージを完了する。
6. Worker が `notify(type="completed")` を送り、Go ハーネスが台帳を更新し、Orchestrator を再起動する。
7. Orchestrator が完了タスクを `archive_task` で `tasks/archive/` へ移し、台帳にはアクティブタスクだけが残る。
8. Web UI / API / tmux console で Orchestrator と Worker のログを確認でき、親リポジトリと worker worktree が分離されていることを確認できる。

## 自動テスト範囲

以下は fake trigger / fake tmux / fake harness を使う受け入れスイートとして扱う。単一の大きな E2E テストではなく、壊れた契約が分かるように境界ごとに分割している。

実行コマンド:

```sh
cd server
go test ./internal/web ./internal/mcp ./internal/orchestrator ./internal/runtime ./internal/ledger
go test ./...
```

`server/internal/mcp/harness_integration_test.go` は `tmux` と `python3` が PATH にある場合だけ fake harness による実 tmux/worktree 起動まで実行する。どちらかがない環境では該当テストは skip されるため、その場合は手動確認範囲の tmux/worktree チェックを必ず実施する。

| 契約 | 自動確認 | 期待結果 |
|---|---|---|
| 自然文タスク追加 | `TestProjectPostTaskAcceptsNaturalLanguageRequest`, `TestPostTasksAcceptsNaturalLanguageRequestWithoutStructuredFields` | `request` または `body` だけで `Natural language intake` が作られ、本文に元の自然文が残り、`allowed_files` は空のまま、Orchestrator trigger が呼ばれる。 |
| project ごとの台帳分離 | `TestProjectScopedTasksUseSelectedProjectLedger`, `TestProjectPostTaskUsesSelectedProjectRuntimeAndNotifiesLedger` | 選択 project の `ledger.md` だけが更新され、別 project は変わらない。 |
| Orchestrator 起動と自然文 intake 指示 | `TestTriggerStartsOrchestratorWithSnapshotAndMCPArgs`, `TestTriggerInstructsOrchestratorToInvestigateNaturalLanguageIntake`, `TestTriggerQueuesWhileWindowActiveAndDrainsAfterIdle` | Orchestrator prompt に project、台帳 snapshot、MCP URL、自然文 intake を調査対象として扱う指示が含まれ、既存 window が active のときはキューイングされる。 |
| Worker 起動 | `TestWorkerHarnessesCompleteThroughMCPIntegration`, `TestBuildWorkerPromptFromTaskUsesTaskRestrictions`, `TestHandleSpawnWorkerRejectsBranchWithoutTaskID` | fake harness で `spawn_worker` から worktree 作成、tmux window 作成、worker prompt 送信、MCP worker endpoint 注入まで到達する。branch は task ID を含まないと拒否される。 |
| Worker notify と台帳更新 | `TestQueueRunProcessesEventsFIFOAndTriggers`, `TestQueueProcessCompletedClearsBlockedReason`, `TestHandleNotifyCompletedThenArchivePreservesMergeMetadata` | `notify(completed)` 後に status が `completed`、`pr_url` が台帳へ反映され、`merge_commit` は archive 用 metadata として保持され、Orchestrator trigger が再度呼ばれる。 |
| split / blocked の派生フロー | `TestWorkerSplitRequestThroughMCPIntegration`, `TestQueueProcessSplitRequestCreatesChildrenAndSplitsParent`, `TestQueueProcessBlockedClearsOmittedReason` | split_request は親を `split` にして子タスクを追加し、blocked は reason を更新する。 |
| archive | `TestArchiveMovesFile`, `TestArchiveCompletedOnly`, `TestArchiveAlreadyArchivedTaskIsIdempotent`, `TestHandleArchiveTaskAlreadyArchivedIsIdempotent` | completed だけが archive に移動し、台帳から消え、二重 archive は安全に扱われる。 |
| tmux console 表示 | `TestProjectOrchestratorLogWebSocketStreamsProjectWindow`, `TestWorkerLogWebSocketStreamsLines`, `TestProjectWorkerFollowupSendsToActiveWorkerWindow` | WebSocket が `{project}-orchestrator` と worker window の行を配信し、followup は active worker window に送信される。 |
| worktree 分離と履歴保護 | `TestCreateCreatesDedicatedBranchWorktree`, `TestDeleteTaskBranchIfSafeSkipsDefaultBranch`, `TestDeleteTaskBranchIfSafeSkipsRemoteBranchAsPullRequestRisk`, `TestDefaultWorkerCleanerSkipsUnsafeDefaultBranchDelete` | worker は専用 worktree/branch で動き、default branch や remote/upstream 付き branch は cleanup で削除されない。 |

不足分:

- REST から自然文を追加し、実 Orchestrator agent が調査してから `spawn_worker` し、実 Worker が GitHub PR を作成・マージするところまでは自動テストでは再現しない。これは agent harness、GitHub 認証、リモートリポジトリ、実 `gh-review-hook` に依存するため手動確認範囲とする。
- ブラウザ上の実 UI 操作、実 tmux pane の目視、GitHub PR merge 後の remote 状態確認は自動テストでは代替しない。

## 手動確認範囲

前提:

- 対象 project が `~/.config/ccx-t2/config.yaml` か `--config` 指定の設定に登録されている。
- `git`, `tmux`, `gh`, `gh-review-hook`、利用する orchestrator / worker harness CLI が PATH にある。
- GitHub token と `gh auth status` が対象リポジトリの PR 作成・マージに十分な権限を持つ。
- 確認用リポジトリの default branch が clean で、作業中の変更はない。

### 1. サーバー起動

```sh
cd server
go run ./cmd/ccx --config /path/to/acceptance-config.yaml
```

期待結果:

- HTTP server が設定済み host/port で起動する。
- tmux session が存在しない場合は作成される。
- 対象 project runtime がロードされ、`/api/projects` に対象 project が出る。

確認コマンド:

```sh
curl -s http://127.0.0.1:8080/api/projects | jq .
tmux list-sessions | grep ccx-t2
```

### 2. 自然文タスク投入

API で確認する場合:

```sh
curl -sS -X POST http://127.0.0.1:8080/api/projects/<project_slug>/tasks \
  -H 'Content-Type: application/json' \
  -d '{"request":"README のセットアップ手順が最新の実装と一致しているか調べ、必要なら修正してテストしてください。"}' | jq .
```

ブラウザで確認する場合:

- Web UI を開き、対象 project を選択する。
- Add Task の入力欄に自然文だけを入れて追加する。
- タスク一覧に `Natural language intake` が表示され、本文に元の自然文が残っていることを確認する。

台帳確認:

```sh
grep -n "Natural language intake" <repo_path>/tasks/ledger.md
```

期待結果:

- API は `201 Created` または project scoped route では `202 Accepted` を返す。
- response の `orchestrator_triggered` が true になる。
- `allowed_files` / `forbidden_files` が未指定のまま保存される。

### 3. Orchestrator console と調査結果

確認コマンド:

```sh
tmux list-windows -t ccx-t2
tmux capture-pane -pt ccx-t2:<project_slug>-orchestrator -S -200
```

Web UI で確認する場合:

- Orchestrator console を開く。
- 自然文 intake の task ID、台帳 snapshot、調査指示、MCP endpoint が表示されることを確認する。

期待ログ:

- `Natural language intake` を worker-ready とみなさず、調査対象として扱う旨の指示が見える。
- Orchestrator が `update_task` / `create_task` / `split_task` のいずれかで、実装範囲、禁止範囲、検証方法を台帳に残す。
- worker-ready なタスクに対して `spawn_worker` が呼ばれる。

### 4. Worker 起動、tmux console、worktree 分離

確認コマンド:

```sh
tmux list-windows -t ccx-t2 | grep "<project_slug>-worker-"
git -C <repo_path> worktree list
```

期待結果:

- worker window 名は `<project_slug>-worker-<task_id>` である。
- worktree path は設定した `runtime.worktree_base` 配下に作成される。
- worker branch は task ID を delimiter-bounded segment として含む。
- 親リポジトリの checkout branch は default branch のまま、または確認開始時の branch のままである。

worker prompt / console の確認観点:

```sh
tmux capture-pane -pt ccx-t2:<project_slug>-worker-<task_id> -S -200
```

- `Worktree path` が専用 worktree を指している。
- `Work only inside the Worktree path above` の指示が含まれる。
- `Do not rewrite history` と `Never force push` の指示が含まれる。
- validation command と MCP worker endpoint が含まれる。

### 5. PR 作成、review hook、マージ

Worker が実行することを期待するコマンド:

```sh
default_branch="$(gh repo view --json defaultBranchRef --jq .defaultBranchRef.name)"
gh pr create --draft --base "$default_branch" --head <worker_branch>
gh-review-hook <pr_number>
gh pr merge <pr_number> --merge --delete-branch
```

期待ログ:

- targeted validation と full validation の結果が worker console に出る。
- `gh-review-hook <pr_number>` が `exit 0` で完了したことが出る。
- PR URL、PR number、merge commit が worker の最終報告に出る。
- force push や history rewrite を行ったログがない。

GitHub 確認:

```sh
gh pr view <pr_number> --json state,mergedAt,mergeCommit,url
git ls-remote origin <worker_branch>
```

期待結果:

- `state` は `MERGED`。
- `mergeCommit.oid` が存在する。
- merge 時に branch deletion を選んだ場合、remote worker branch は残っていない。残っていても履歴は rewrite されていない。

### 6. notify、台帳更新、archive

Worker は PR merge と `gh-review-hook` 成功後だけ `notify(type="completed")` を送る。payload は少なくとも次を含む。

```json
{
  "project_slug": "<project_slug>",
  "task_id": "<task_id>",
  "worker_id": "<project_slug>-worker-<task_id>",
  "pr_url": "https://github.com/<owner>/<repo>/pull/<number>",
  "merge_commit": "<merge_commit_sha>"
}
```

確認コマンド:

```sh
grep -n "<task_id>" <repo_path>/tasks/ledger.md
grep -R "<task_id>" <repo_path>/tasks/archive
grep -R "<merge_commit_sha>" <repo_path>/tasks/archive
```

期待結果:

- notify 直後は対象タスクが `completed` になり `pr_url` が残る。
- Orchestrator 再起動後、`archive_task` により対象タスクは `tasks/ledger.md` から消える。
- `tasks/archive/` の対象ファイルに `pr_url` と `merge_commit` が残る。
- 未完了タスクがある場合だけ heartbeat / Orchestrator trigger が継続する。

### 7. console と Web UI の最終確認

Web UI:

- Tasks 画面で completed/archive 済みタスクがアクティブ一覧から消えている。
- Orchestrator console に worker completed event 後の再起動ログがある。
- Worker dashboard で対象 worker が終了済み、または active worker 一覧から消えている。

tmux:

```sh
tmux list-windows -t ccx-t2
```

期待結果:

- 完了済み worker window は cleanup される。
- Orchestrator window は対象 project ごとに 1 つだけである。

worktree:

```sh
git -C <repo_path> worktree list
test ! -d <runtime.worktree_base>/<project_slug>-<task_id>
```

期待結果:

- 完了済み worker worktree は削除されている。
- default branch や remote/upstream 付き branch は cleanup によって削除されていない。

## 受け入れ判定

合格条件:

- 自動テストコマンドが成功する。tmux/python3 不在で fake harness integration が skip された場合は、その事実を記録し、手動 tmux/worktree 確認を実施する。
- 自然文投入から Orchestrator 調査、worker-ready task 化、Worker 起動、PR merge、notify、archive までの証跡が残る。
- PR URL、PR number、merge commit、`gh-review-hook` exit 0、validation 結果が worker の完了報告に含まれる。
- `tasks/ledger.md` はアクティブタスクだけを保持し、完了タスクは `tasks/archive/` に移動している。
- tmux console と Web UI console で Orchestrator / Worker のログが確認できる。
- worker は専用 worktree で作業し、親リポジトリや default branch の履歴を壊していない。

残余リスク:

- 実ハーネスの CLI 出力や認証状態は外部依存のため、自動テストでは完全には固定できない。
- GitHub の branch protection、review requirement、merge method の設定差分は手動確認時の環境に依存する。
- ブラウザでの詳細な視覚回帰はこの受け入れ計画の範囲外であり、console 表示と API/WebSocket 契約の確認を主対象とする。
