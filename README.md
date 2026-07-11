# ccx-t2

`ccx-t2` は、複数プロジェクトの tmux terminal をブラウザから扱う WebShell と、
coding-agent の task/worktree/runtime を支える Go サービスです。普段の操作面は
terminal-first で、左の project sidebar、project ごとの shell 一覧、上部の tab、
1つの full-height terminal で構成されます。

## WebShell の使い方

- sidebar で project を選ぶと、その project に属する tmux window だけが表示されます。
- `New shell` は project repository を cwd にした shell window を作成します。
- sidebar または上部 tab で terminal を切り替えます。terminal ごとの scrollback と入力先は分離されます。
- shell tab は status bar の `Close` で閉じられます。orchestrator / worker window は通常の terminal として表示・操作できますが、harness lifecycle を壊さないよう close は保護されます。
- terminal stream が切れると自動的に再接続を続けます。`Reconnect` で即時再試行でき、browser が online に戻った時や visible になった時にも復帰を試みます。
- window がなくなった場合や認証に失敗した場合は、再試行中とは区別して状態を表示します。

接続時は tmux pane の snapshot と後続の live output を順序どおり受け取ります。入力は選択中の
project/window の WebSocket にだけ送り、表示領域の変更は tmux pane の rows/columns に同期します。
再接続後は snapshot から表示を回復し、最後に測定した terminal size を再送します。

## セキュリティ境界

- project slug と window ID は server 側で検証され、別 project の window は列挙・接続・削除できません。
- UI が作成した project shell だけが generic terminal API から削除できます。
- orchestrator / worker window は discoverable な protected terminal で、generic delete の対象外です。
- API token は HTTP の `Authorization: Bearer` と WebSocket query に使用されます。config API は secret をマスクします。
- terminal は shell と同じ権限で動作します。信頼できる host/network で稼働させ、外部公開時は TLS、認証、reverse proxy の access control を設定してください。

## リポジトリ構成

```text
.
├── docs/      要件、実装計画、受け入れシナリオ
├── server/    Go backend、terminal REST/WS、埋め込み Web UI
└── web/       Vite + React + TypeScript + xterm WebShell
```

## 必要なもの

- Go 1.23 以上
- Node.js と npm
- git
- tmux
- 利用する agent harness CLI（`claude`, `codex`, `opencode`, `cursor-agent`）

## セットアップと起動

```sh
cd server
go test ./...
go run ./cmd/ccx
```

既定では `~/.config/ccx-t2/config.yaml` を読みます。存在しない場合は project が空の config を
生成します。project は config API または設定ファイルで登録してください。

```yaml
server:
  host: 127.0.0.1
  port: 8080
  mcp_secret: ${CCX_MCP_SECRET}

runtime:
  tmux_session: ccx-t2
  worktree_base: /path/to/worktrees

projects:
  my-project:
    repo_path: /path/to/my-project
```

Web UI の開発時は別 terminal で次を実行します。

```sh
cd web
npm install
npm run dev
```

Vite は `127.0.0.1:5173` で起動し、`/api` と `/ws` を `127.0.0.1:8080` へ proxy します。

## Build と embedded assets

配布時は `server/internal/webui/dist` の asset が Go binary に埋め込まれます。`--web-dir`
（既定 `web/dist`）が存在する場合だけ外部 directory を優先するため、開発 build の確認にも使えます。

Web UI を変更した時は clean build を同期し、一致を検証します。

```sh
cd web
npm run build
npm run embedded:sync
npm run embedded:check
```

## Terminal API

- `GET /api/projects`
- `GET /api/projects/{slug}/terminals`
- `POST /api/projects/{slug}/terminals`
- `DELETE /api/projects/{slug}/terminals/{window}`
- `GET /ws/projects/{slug}/terminal/{window}`

WebSocket の client frame は `input` と `resize`、server frame は terminal `chunk` と `error` を扱います。
task/worker/config/MCP API は harness compatibility のため引き続き提供されますが、primary WebShell
surface ではありません。

詳細は [要件](docs/requirements.md)、[実装計画](docs/implementation-plan.md)、
[手動受け入れシナリオ](docs/acceptance-scenario.md) を参照してください。

## ライセンス

MIT License。詳細は [LICENSE](LICENSE) を参照してください。
