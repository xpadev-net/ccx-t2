# Plan: cmux-like project WebShell

- status: in_progress
- generated: 2026-07-11
- last_updated: 2026-07-11
- work_type: code
- execution: task-pr-orchestrator / separate Codex worktree threads
- worker_model: gpt-5.6-luna
- worker_reasoning: high

## Goal
- Make the Web UI a stable terminal-first workspace: projects in the existing sidebar, project-scoped tmux shell tabs across the top, and one full-height interactive terminal.
- Remove harness/task orchestration concepts from the primary UI while retaining backend compatibility.

## Definition of Done
- Projects group their own discoverable tmux windows; users can create, switch, reconnect to, and close ordinary shell tabs.
- Harness-created orchestrator/worker windows appear as protected ordinary tabs rather than special dashboards.
- Terminal output has a coherent snapshot/live handoff, idle keepalive, bounded backpressure behavior, indefinite recovery, correct input routing, and resize synchronization.
- All worker PRs pass worker review/hook gates, then orchestrator deep review, hook, full tests, browser E2E/visual review, and merge.

## Scope / Non-goals
- Scope: tmux window primitives, project terminal REST/WS APIs, terminal transport reliability, terminal-first React UI, documentation, embedded assets.
- Non-goals: task-ledger/harness dashboard replacement, tmux pane splitting, scheduler/MCP behavior changes.

## Product Decisions
- A tab maps 1:1 to a tmux window in the configured shared session.
- Project ownership is enforced by the existing project window prefix.
- UI-created windows use an explicit project shell prefix/marker and are closable; orchestrator/worker windows are visible but protected.
- Task CRUD/settings APIs remain compatible but disappear from the primary WebShell surface.
- The existing project sidebar is the visual baseline; the content area follows the supplied cmux screenshot: compact dark chrome, horizontal tabs, full-height terminal.

## Current Mismatches / Root Causes
- `web/src/main.tsx` renders task editor, create form, orchestrator dashboard, worker dashboard, follow-up, and settings as primary content.
- Orchestrator and worker terminals use separate state/socket paths instead of one project terminal model; worker terminal is read-only in the UI.
- `openReconnectingWebSocket` exhausts a finite retry list and has no online/visibility/manual recovery path.
- `server/internal/web/ws.go` captures before subscribing, allowing output between snapshot and live subscription to be missed.
- WebSockets lack application keepalive/read deadlines, so proxy/browser half-open connections can appear healthy.
- Shared stream subscribers silently drop raw terminal chunks when their channel is full, potentially corrupting xterm state.

## Task Ledger

### Task_1: tmux project-window primitives
- status: complete
- type: impl
- branch: `codex/webshell-tmux-primitives`
- thread: `019f4d7d-a6b6-7642-8607-3ca97d2cb2c1`
- pr: `#65` https://github.com/xpadev-net/ccx-t2/pull/65
- head: `32e2bccfb2248d983bf30912b66554a30efceecf`
- merge_commit: `d58fa7d8423cee8fde89967662f2f7eae90428fa`
- completion evidence: parent `$deep-review` and independent review CLEAN; parent `gh-review-hook 65` exit 0; normal/race/full `-race -count=20` (780 tests)/vet/diff checks passed; all seven GitHub checks green; worker archival initiated.
- owns:
  - `server/internal/tmux/tmux.go`
  - `server/internal/tmux/tmux_test.go`
- forbidden:
  - `server/internal/web/**`
  - `web/**`
  - documentation and embedded assets
- depends_on: []
- implementation detail:
  - Add context-aware window enumeration returning stable tmux name and metadata needed by the web layer.
  - Add creation of uniquely named project shell windows with the project repository as cwd and a normal interactive shell.
  - Add strict name/prefix validation helpers; retain existing APIs and command injection safety.
  - Reuse safe kill primitives but leave policy decisions (which window is closable) to the web layer.
- acceptance:
  - Enumeration parses spaces/special characters safely and distinguishes missing session from command failure.
  - Creation rejects invalid/foreign names, avoids collisions deterministically, and starts in the requested repository.
  - Cancellation/timeouts propagate through every new tmux command.
  - Existing tmux behavior and tests remain compatible.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test ./internal/tmux`
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test -race ./internal/tmux`
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0 before merge-ready report.

### Task_2: reliable terminal WebSocket transport
- status: split_in_progress
- type: impl
- branch: `codex/webshell-ws-stability`
- thread: `019f4d7d-a6b5-71f1-acda-e0e404f28bfb`
- owns:
  - `server/internal/web/ws.go`
  - `server/internal/web/ws_test.go`
- forbidden:
  - `server/internal/web/server.go`
  - `server/internal/web/server_test.go`
  - `server/internal/tmux/**`
  - `web/**`
- depends_on: []
- implementation detail:
  - Subscribe to the shared pane stream before snapshot capture and define a deterministic snapshot/live boundary.
  - Add websocket ping writes, pong/read deadlines, serialized writes, and clean shutdown without goroutine leaks.
  - Replace silent terminal-byte drop with an explicit slow-client policy: close/resync signal rather than corrupted continuation.
  - Preserve multi-subscriber single-pipe behavior and existing ledger websocket behavior.
- acceptance:
  - Tests reproduce and prevent snapshot/live gaps and raw-byte loss ambiguity.
  - Idle healthy clients remain connected; dead peers are reclaimed within a bounded interval.
  - Concurrent ping/chunk/error writes never violate Gorilla's single-writer requirement.
  - Slow clients cannot corrupt the shared stream or starve healthy subscribers.
  - Cleanup is idempotent and race-clean.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test ./internal/web`
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test -race ./internal/web`
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0 before merge-ready report.

#### Split decision
- Keep the accepted writer serialization, ping/pong deadlines, cleanup, shared-stream fanout, bounded backpressure, healthy-subscriber isolation, and deterministic slow-client reconnect behavior in Task_8 / PR #66.
- Move the unprovable snapshot/live handoff into Task_9 because the existing independent `PipeBytesFunc` and `CapturePaneFunc` expose no watermark or atomic boundary.
- Task_2 is complete only when both Task_8 and Task_9 are merged and validated.

### Task_8: mergeable WebSocket transport stability subset
- status: complete
- type: impl
- branch: `codex/webshell-ws-stability`
- thread: `019f4d7d-a6b5-71f1-acda-e0e404f28bfb`
- pr: `https://github.com/xpadev-net/ccx-t2/pull/66`
- merged_head: `eadde1ebf652663cd628058696111c1e58cd7fdc`
- merge_commit: `6439fb12a3d6e3185d7d6280cbd1ab04ff398189`
- evidence: worker and orchestrator normal/race/vet/stress validation passed; worker and orchestrator `gh-review-hook` exited 0; Greptile and independent Reviewer APPROVED; all six GitHub checks passed.
- owns:
  - `server/internal/web/ws.go`
  - `server/internal/web/ws_test.go`
- forbidden:
  - `server/internal/web/server.go`
  - `server/internal/web/server_test.go`
  - `server/internal/tmux/**`
  - `web/**`
- depends_on: []
- implementation detail:
  - Retain serialized websocket JSON/ping writes, pong/read deadlines, idempotent cleanup, shared single-pipe fanout, bounded per-subscriber backpressure, healthy-subscriber isolation, and explicit slow-client reconnect/error behavior.
  - Remove `snapshotPending` / begin-finish snapshot / queue-drain heuristics and their invalid overlap test.
  - Restore the pre-existing capture-before-subscribe sequence so this PR does not introduce duplicates or additional loss; document the existing handoff gap as Task_9 rather than claiming it fixed here.
- acceptance:
  - Concurrent writers and pings obey Gorilla's single-writer contract.
  - Dead peers are reclaimed and idle healthy peers remain connected.
  - Slow subscribers are explicitly disconnected for resync without corrupting healthy subscribers.
  - No heuristic snapshot queue drain remains and the PR description does not claim no-gap/no-duplicate attachment.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test ./internal/web && go test -race ./internal/web && go vet ./internal/web`
  - kind: command
    required: true
    owner: worker
    detail: focused race-count stress for writer, eviction, healthy-subscriber isolation, and dead-peer cleanup
  - kind: review
    required: true
    owner: reviewer
    detail: `$deep-review` confirms the reduced PR preserves prior snapshot semantics while improving independent transport lifecycle concerns.

### Task_9: atomic tmux pane attachment and web integration
- status: complete
- type: impl
- branch: `codex/webshell-atomic-pane-attach`
- thread: `019f4ec5-c782-7163-8fd4-41dac81831b7`
- pr: `#67` (`https://github.com/xpadev-net/ccx-t2/pull/67`)
- head: `dabc006837993f09a96d1226fd50068e39f43cdd`
- merge_commit: `b32a8e9b12b9073bd8bb39b9a3b123bf16dc6eb3`
- owns:
  - `server/internal/tmux/tmux.go`
  - `server/internal/tmux/tmux_test.go`
  - `server/internal/web/server.go`
  - `server/internal/web/server_test.go`
  - `server/internal/web/ws.go`
  - `server/internal/web/ws_test.go`
- forbidden:
  - `web/**`
  - scheduler, MCP, ledger, worker/orchestrator semantics, docs, embedded assets
- depends_on: [Task_1, Task_8]
- implementation detail:
  - Define one tmux-owned pane attachment contract that returns initial terminal state plus an ordered live stream with a formal boundary/watermark.
  - Implement the boundary using one tmux-side serialized/control-mode transaction or another mechanism that proves every byte is represented exactly once; sequential independent `pipe-pane` and `capture-pane` calls are insufficient.
  - Wire the atomic attachment through web dependencies and consume it in the terminal websocket without byte comparison or queue-drain guesses.
- acceptance:
  - Output before the boundary is represented by the snapshot exactly once.
  - Output after snapshot sampling but before the attach call returns is delivered live exactly once.
  - Repeated bytes, arbitrary chunking, ANSI/control sequences, cancellation, slow consumers, and cleanup preserve the boundary contract.
  - Web handler tests prove no gaps and no duplicates under forced boundary schedules and race stress.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test ./internal/tmux ./internal/web`
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test -race ./internal/tmux ./internal/web`
  - kind: review
    required: true
    owner: reviewer
    detail: Independent review of tmux ordering proof, cancellation, cleanup, realtime semantics, and deterministic boundary tests.

### Task_3: project terminal REST and generic WS API
- status: complete
- type: impl
- branch: `codex/webshell-terminal-api`
- thread: `019f4f92-1b39-7701-8c78-01cde7f5cd7c`
- pr: `#68` (`https://github.com/xpadev-net/ccx-t2/pull/68`)
- head: `317ea34c6c4748ddc6c430348d438b1006274cc4`
- merge_commit: `daf1d379d5f55f71c5ca38eb89abd0b12bb6f584`
- owns:
  - `server/internal/web/server.go`
  - `server/internal/web/server_test.go`
- forbidden:
  - `server/internal/web/ws.go`
  - `server/internal/tmux/**`
  - `web/**`
- depends_on: [Task_1, Task_8, Task_9]
- implementation detail:
  - Add project routes for terminal list/create/delete and `/ws/projects/{slug}/terminal/{window}`.
  - Return stable DTO fields: id/window, display title, kind (`shell|orchestrator|worker`), active/available state, and `closable`.
  - Enforce project prefix and protect harness-managed windows from generic delete.
  - Refresh/list behavior must tolerate windows disappearing between REST and websocket operations.
- acceptance:
  - Cross-project and malformed window access returns 4xx without invoking tmux mutations.
  - UI-created shells are created in the configured repo path and can be deleted idempotently.
  - Orchestrator/worker windows list as protected and remain compatible with legacy routes.
  - Generic websocket supports snapshot/live output, raw input, and resize through the hardened transport.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test ./internal/web`
  - kind: command
    required: true
    owner: worker
    detail: `cd server && go test -race ./internal/web`
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0.

### Task_4: frontend terminal client and reconnect state machine
- status: complete
- type: impl
- branch: `codex/webshell-terminal-client`
- thread: `019f4fc6-9813-7012-a7fd-441fdc78fef5`
- pr: `#69` (`https://github.com/xpadev-net/ccx-t2/pull/69`)
- head: `b783e3fbc6d726615ad81c4b64122740f379bd08`
- merge_commit: `1dad20bc56a5a48209a606ac4eed0dcb838de40a`
- owns:
  - `web/src/terminal/**`
- forbidden:
  - `web/src/main.tsx`
  - `web/src/styles.css`
  - backend, docs, package manifests, embedded assets
- depends_on: [Task_3]
- implementation detail:
  - Introduce typed terminal/project API functions and a reusable per-terminal websocket controller/hook.
  - Implement bounded exponential delay with indefinite retries, jitter, manual retry, and online/visibility wake recovery.
  - Guard every callback/send by project+window generation so stale sockets cannot append output or receive input.
  - Expose explicit `idle|connecting|open|retrying|missing|unauthorized|failed` state and resize replay after reconnect.
- acceptance:
  - Rapid project/tab changes cannot route stale events or input to the previous window.
  - Teardown cancels timers/listeners/sockets and reconnect never duplicates live connections for one controller.
  - Retry state is deterministic and testable without depending on the UI layout.
  - API errors preserve actionable status/details.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd web && npm run build`
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0.

### Task_5: cmux-style terminal-first workspace
- status: complete
- pr: `#70` (`https://github.com/xpadev-net/ccx-t2/pull/70`)
- merged_head: `f5487dd17f4be640320afd5edc272bcecc8f51fd`
- merge_commit: `ba48493fa61314ba824a2c0881d3e616532bd904`
- type: impl
- branch: `codex/webshell-cmux-ui`
- thread: `019f5015-78ee-7e21-a1aa-626735d4c5b7`
- owns:
  - `web/src/main.tsx`
  - `web/src/styles.css`
- forbidden:
  - `web/src/terminal/**`
  - backend, docs, package manifests, embedded assets
- depends_on: [Task_4]
- implementation detail:
  - Replace dashboard navigation with the retained project sidebar, nested terminal entries/status, top tab strip, and one full-height xterm surface.
  - Add new tab, close closable tab, reconnect, refresh, connection state, empty/missing terminal, and keyboard-focus interactions.
  - Keep xterm instances/state isolated per tab where practical; fit on selected tab, container resize, and reconnect.
  - Use dark compact cmux-inspired styling without copying unrelated Codex/harness chrome.
- acceptance:
  - Project sidebar remains recognizable and clearly groups its terminals.
  - Desktop `1440x900` prioritizes a full-height terminal; narrow `900x700` remains usable.
  - All discovered windows use the same interaction model; no harness-specific primary panels remain.
  - New/close/switch/reconnect actions have accessible labels, focus states, and safe confirmations/capability checks.
  - Switching tabs/projects never visually mixes scrollback or sends input to a stale terminal.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd web && npm run build`
  - kind: ui_probe
    required: true
    owner: worker
    detail: Browser probe at `1440x900` and `900x700`, including two tabs and reconnect state.
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0.

### Task_6: product documentation and embedded assets
- status: in_progress
- type: docs
- branch: `codex/webshell-docs-assets`
- thread: `client-new-thread:96f1ac2b-32f3-4f4a-928c-bb0ec8adf81d` (replacement worktree setup queued)
- owns:
  - `README.md`
  - `docs/requirements.md`
  - `docs/implementation-plan.md`
  - `docs/acceptance-scenario.md`
  - `server/internal/webui/dist/**`
- forbidden:
  - product source and tests
- depends_on: [Task_5]
- implementation detail:
  - Align product language with WebShell-first intent and explain terminal lifecycle/recovery/security boundaries.
  - Replace harness-dashboard acceptance steps with project/tab/input/resize/reconnect/protected-window flows.
  - Build and synchronize embedded UI artifacts from the merged frontend source.
- acceptance:
  - Documentation no longer describes the primary UI as task/orchestrator/worker dashboards.
  - Manual acceptance is executable and covers all critical terminal flows.
  - Embedded assets exactly match a clean frontend build.
- validation:
  - kind: command
    required: true
    owner: worker
    detail: `cd web && npm run build && npm run embedded:sync && npm run embedded:check`
  - kind: review
    required: true
    owner: worker
    detail: `$deep-review`, independent subagent review, and `gh-review-hook` exit 0.

### Task_7: integrated review and browser acceptance
- status: waiting
- type: review
- branch: none
- thread: reviewer to be created after Task_6
- owns: []
- depends_on: [Task_6]
- acceptance:
  - Independent reviewer status is APPROVED with no unresolved P0-P2 findings.
  - Browser evidence exists under `.playwright-cli/` for both viewports and all specified flows.
  - Orchestrator `$deep-review`, `gh-review-hook`, full Go/race tests, frontend build, and embedded check pass on current `master`.
- validation:
  - kind: command
    required: true
    owner: orchestrator
    detail: `cd server && go test ./... && go test -race ./... -timeout 120s`
  - kind: command
    required: true
    owner: orchestrator
    detail: `cd web && npm run build && npm run embedded:check`
  - kind: e2e
    required: true
    owner: reviewer
    detail: Execute the E2E spec using `playwright-cli` and retain screenshots/network/console evidence.

## Task Waves
- Wave 1a (parallel): [Task_1, Task_8]
- Wave 1b (atomic attachment after both merge): [Task_9]
- Wave 2 (sequential integration): [Task_3]
- Wave 3 (sequential): [Task_4]
- Wave 4 (sequential): [Task_5]
- Wave 5 (sequential): [Task_6]
- Wave 6 (independent acceptance): [Task_7]

## E2E / Visual Validation Spec
- provider: `playwright-cli`
- artifact_root: `.playwright-cli/`
- base_url: `http://127.0.0.1:8080`
- readiness: `GET /api/projects` succeeds and WebShell renders.
- flows:
  - Project A: create two shells, execute distinct commands, switch repeatedly, verify isolated output/input, close one shell.
  - Harness windows: verify orchestrator/worker tabs are discoverable, interactive where allowed, and not generically closable.
  - Project isolation: switch to project B; verify A's windows/output cannot be addressed.
  - Terminal fidelity: type, paste multibyte text, run alternate-screen command, resize, and verify pane dimensions/rendering.
  - Recovery: disconnect/restart server or network, observe retry, recover snapshot/live stream, continue input without page reload.
- viewports: `1440x900`, `900x700`
- required evidence: initial workspace, multi-tab state, second project, retry state, recovered terminal screenshots; console errors and failed network request summary.

## Merge / Safety Policy
- Repository trust boundary: `origin` is `xpadev-net/ccx-t2`; autonomous worker PR creation and orchestrator merge are permitted.
- Every worker uses its own Codex worktree and must never merge its PR.
- Parent thread changes only this ledger/plan, performs merge gates/merges, and archives completed workers.
- Existing open-PR branches must never be force-pushed or history-rewritten.
- A dependent task starts only after required predecessor PRs are merged and its branch is created from current remote `master`.

## Progress Log
- 2026-07-11: Initial four-task draft created.
- 2026-07-11: User approved execution and requested finer decomposition plus task-pr-orchestrator, separate threads, `luna high`.
- 2026-07-11: Expanded to seven tasks with file ownership, dependencies, acceptance, validation, PR gates, and E2E spec. Wave 1 pending delegation.
- 2026-07-11: Wave 1 delegated in separate worktrees using `gpt-5.6-luna` / `high`. Task_1 startup system error received one required resume; Task_2 passed startup stability check.
- 2026-07-11: Task_2 split after repeated parent deep-review proved the independent snapshot/pipe interfaces cannot establish an atomic no-gap/no-duplicate boundary. PR #66 continues as Task_8 reduced transport stability; Task_9 owns the new tmux attachment contract after Task_1 and Task_8 merge.
- 2026-07-11: Task_8 merged via PR #66 at `6439fb12a3d6e3185d7d6280cbd1ab04ff398189` after reduced-scope parent deep-review APPROVED, independent Reviewer APPROVED, hook exit 0, normal/race/vet/stress passes, and all GitHub checks green. Worker archival initiated.
- 2026-07-11: Task_1 merged via PR #65 at `d58fa7d8423cee8fde89967662f2f7eae90428fa` after parent and independent deep-review CLEAN, hook exit 0, normal/race/full 20x race stress/vet passes, and all GitHub checks green. Task_9 dependency gate opened.
- 2026-07-11: Task_9 started in separate worktree thread `019f4ec5-c782-7163-8fd4-41dac81831b7` on `codex/webshell-atomic-pane-attach` using `gpt-5.6-luna` / `high`.
- 2026-07-11: Task_9 merged via PR #67 at `b32a8e9b12b9073bd8bb39b9a3b123bf16dc6eb3` after exact-head parent and two independent deep-reviews were CLEAN, orchestrator hook exit 0, 179 normal/race tests, vet, 260 focused race-stress cases, diff-check, and all GitHub checks passed. Task_3 dependency gate opened.
- 2026-07-11: Task_3 started in separate worktree thread `019f4f92-1b39-7701-8c78-01cde7f5cd7c` on `codex/webshell-terminal-api` using `gpt-5.6-luna` / `high`.
- 2026-07-11: Task_3 merged via PR #68 at `daf1d379d5f55f71c5ca38eb89abd0b12bb6f584` after stable tmux window-ID resolution closed prefix-collision findings; exact-head parent and two independent deep-reviews were CLEAN, hook exit 0, 149 normal/race tests, vet, 280 focused race-stress cases, diff-check, and all GitHub checks passed. Task_4 dependency gate opened.
- 2026-07-11: Task_4 started in separate worktree thread `019f4fc6-9813-7012-a7fd-441fdc78fef5` on `codex/webshell-terminal-client` using `gpt-5.6-luna` / `high`.
- 2026-07-11: Task_4 merged via PR #69 at `1dad20bc56a5a48209a606ac4eed0dcb838de40a` after target identity plus generation closed the React pre-passive-effect stale-action race; exact-head parent and two independent deep-reviews were CLEAN, build and diff-check passed, fresh hook exited 0, and all GitHub checks were green. Task_5 dependency gate opened.
- 2026-07-11: Task_5 started in separate worktree thread `019f5015-78ee-7e21-a1aa-626735d4c5b7` on `codex/webshell-cmux-ui` using `gpt-5.6-luna` / `high`, with required desktop/narrow browser probes and the user-provided cmux reference image.
- 2026-07-11: Task_5 merged via PR #70 at `ba48493fa61314ba824a2c0881d3e616532bd904` after parent and independent deep-review were CLEAN, current-head build/embedded check/hook and all GitHub checks passed, and Reviewer E2E at `1440x900`/`900x700` approved project/tab lifecycle, credential isolation, stale-scrollback prevention, protected terminals, focus, errors, and responsive overflow. Task_6 dependency gate opened; Task_5 archival was requested.
- 2026-07-11: Task_5 worker thread archived. Task_6 separate `gpt-5.6-luna` / `high` worktree setup queued as `client-new-thread:6fe51544-4be8-4da7-85c6-26cbf26bd227` on delegated branch `codex/webshell-docs-assets`; startup stability check pending thread materialization.
- 2026-07-11: Original Task_6 queued setup did not materialize and no matching thread/worktree was discoverable after repeated checks. Replacement `gpt-5.6-luna` / `high` worktree setup queued as `client-new-thread:96f1ac2b-32f3-4f4a-928c-bb0ec8adf81d`; startup stability check remains pending.

## Decision Log
- 2026-07-11: WebShell is the product UI; harness/task concepts remain backend-compatible but are removed from primary navigation.
- 2026-07-11: Split stream reliability from REST routing and tmux primitives to isolate concurrency/security review.
- 2026-07-11: Serialize frontend client then UI because both establish contracts consumed by `main.tsx`; avoiding parallel integration churn outweighs speed.
- 2026-07-11: Approved Task_2 decomposition. Rejected further websocket-only queue-drain heuristics because capture sampling and pipe delivery are observationally ambiguous without a tmux-owned watermark.
- 2026-07-11: Approved the minimal Task_5 generated-asset exception because CI requires embedded assets to match frontend source in the same PR; Task_6 retains documentation ownership and final post-merge rebuild/sync verification.
