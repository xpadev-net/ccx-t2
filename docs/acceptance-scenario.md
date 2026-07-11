# WebShell 受け入れシナリオ

project-grouped terminal UI の create / close / switch / input / resize / reconnect と、
harness window の保護を実環境で確認する。確認中は browser console、失敗した network request、
server log を記録し、`1440x900` と `900x700` の両 viewport で screenshot を残す。

## 1. 前提と起動

- `git`, `tmux`, Go 1.23+, Node.js, npm が PATH にある。
- config に repository path の異なる project A / B を登録する。
- project A には orchestrator/worker window を1つ以上用意する。これらは harness が管理する protected terminal とする。

```sh
cd web
npm run build
npm run embedded:check
git diff --exit-code -- ../server/internal/webui/dist

cd ../server
go run ./cmd/ccx --config /path/to/acceptance-config.yaml
```

```sh
curl -fsS http://127.0.0.1:8080/api/projects | jq .
tmux list-windows -t ccx-t2
```

期待結果:

- `/api/projects` に A / B が返る。
- `http://127.0.0.1:8080` は project sidebar と terminal workspace を表示する。
- task editor、orchestrator dashboard、worker dashboard は primary navigation に表示されない。

## 2. Project A で terminal を2つ作る

1. sidebar で project A を選ぶ。
2. `New shell` を2回実行する。
3. sidebar と上部 tab strip の両方に2つの shell が現れることを確認する。

APIでも確認する:

```sh
curl -fsS http://127.0.0.1:8080/api/projects/<project-a>/terminals | jq .
```

期待結果:

- 各 terminal は一意な `id` / `window` を持ち、`kind` は `shell`、`closable` は `true`。
- tmux window は project A の prefix を持ち、cwd は project A の repository。
- desktop では terminal が残り高さを使う。narrow viewport でも sidebar/tab/terminal が操作でき、tab strip は安全に横スクロールする。

## 3. Input と tab/project isolation

1. shell A1 で `printf 'A1-only\n'` を実行する。
2. shell A2 へ切り替え、`printf 'A2-only\n'` を実行する。
3. sidebar と上部 tab のそれぞれから A1/A2 を10回以上切り替える。
4. multibyte inputとして `printf '日本語-✓\n'` を貼り付ける。
5. project B へ切り替えて shell B1 を作り、`printf 'B1-only\n'` を実行する。

期待結果:

- 入力は現在選択中の project/window にだけ届く。
- A1、A2、B1 の scrollback が混ざらず、切り替え後も各 terminal の出力が保持される。
- project B から project A の window を接続・削除できない。
- keyboard focus と accessible label で terminal切り替え、作成、再接続、closeを操作できる。

## 4. Resize と terminal fidelity

1. shell A1 で `printf '\e[2J\e[Hresize-check\n'` を実行する。
2. browser window を広い状態から狭い状態へ変更し、再び広げる。
3. `stty size` を各段階で実行する。
4. `less`、`top`などの alternate-screen programを起動し、終了する。

```sh
tmux display-message -pt 'ccx-t2:<project-a-shell-window>' '#{pane_width}x#{pane_height}'
```

期待結果:

- xterm が container に fit し、tmux pane の columns/rows が追従する。
- alternate screen、ANSI sequence、multibyte text が破損しない。
- tabを切り替えて戻った後も選択中terminalのsizeが再同期される。

## 5. Close lifecycle

1. A2 の status bar にある `Close` を実行し、確認dialogを承認する。
2. 同じ window を API で再度削除する。

```sh
curl -i -X DELETE \
  http://127.0.0.1:8080/api/projects/<project-a>/terminals/<a2-window>
```

期待結果:

- A2 は sidebar/tab/tmux から消え、別terminalへ安全に選択が移る。
- 既に消えた shell の再削除は安全に扱われ、別windowを削除しない。
- closed terminalへの stale socket/output/input は現在のterminalへ適用されない。

## 6. Protected harness windows

1. project A の orchestrator と worker window を sidebar/tab からそれぞれ開く。
2. 通常の terminal と同じ xterm surface で出力を確認し、許可された session へ入力する。
3. close control がないことを確認する。
4. generic DELETE を直接試す。

```sh
curl -i -X DELETE \
  http://127.0.0.1:8080/api/projects/<project-a>/terminals/<orchestrator-window>
```

期待結果:

- response DTO は `kind: orchestrator` または `kind: worker`、`closable: false`。
- harness window は primary dashboardではなく、project配下のprotected terminal entryとして表示される。
- generic DELETE は4xxを返し、tmux windowは残る。

## 7. Disconnect と recovery

1. A1を表示したままserverを停止するか、browser networkをofflineにする。
2. UIが`retrying`を表示するまで待つ。
3. `Reconnect`で手動再試行する。
4. server/networkを復旧する。offlineの場合はbrowserをonlineに戻す。必要ならtabをbackgroundからvisibleへ戻す。
5. 再接続後に`printf 'after-reconnect\n'`を実行する。

期待結果:

- retryは上限回数で終了せず、bounded exponential delayとjitterで継続する。
- manual reconnect、`online`、visible復帰が待機中の再接続を起こす。
- 認証失敗は`unauthorized`、消失windowは`missing`としてretryingと区別される。
- 復帰時はsnapshotとlive outputに欠落・重複がなく、切断前に測定したsizeが再送される。
- page reloadなしで入力を再開でき、古いsocketは出力や入力を横取りしない。

## 8. Security boundary probes

```sh
curl -i http://127.0.0.1:8080/api/projects/<project-b>/terminals/<project-a-window>
curl -i -X DELETE http://127.0.0.1:8080/api/projects/<project-b>/terminals/<project-a-window>
curl -i http://127.0.0.1:8080/ws/projects/<project-b>/terminal/<project-a-window>
```

認証を有効にしている場合はtokenなし/不正tokenでもlist/create/delete/WSを試す。

期待結果:

- malformed/foreign windowは4xxで拒否され、tmux mutationを起こさない。
- 認証失敗は401となり、secret/tokenはUI、response、logに露出しない。
- slow clientはterminal byteを黙ってdropして継続せず、明示的に切断されsnapshotからresyncする。

## 9. 合格判定

- create / close / switch / input / resize / reconnectの全手順が成功する。
- project isolationとprotected-window delete拒否が確認できる。
- `1440x900` / `900x700` のinitial workspace、multi-tab、project B、retrying、recovered terminalの証跡がある。
- browser consoleに未解決errorがなく、想定外のfailed requestがない。
- clean buildとcommitted embedded assetsが一致し、検査がassetを変更しない。

自動回帰確認:

```sh
cd server
go test ./...
go test -race ./... -timeout 120s

cd ../web
npm run build
npm run embedded:check
```
