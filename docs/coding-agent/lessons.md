# Lessons Log (Coding Agent)

Purpose:
- capture recurring mistakes and the prevention mechanism
- enable “read once, don’t repeat” improvements

## How to use
- Append a new entry after any user correction or significant miss.
- Keep entries short and actionable.
- Promote repeated/high-severity lessons into repo rules, harness migration candidates, troubleshooting notes, or accepted residual-risk records.

## Tags (recommended)
- planning
- validation
- delegation
- review
- ui-e2e
- tooling
- ci
- scope-owns

## Entries

### 2026-07-06 - Orchestrator interaction must be Web shell first

Tags: assumptions/interpretation, ui-e2e

Symptom: The UI change added a separate textarea-style instruction form for the Orchestrator, while the user wanted browser interaction equivalent to a native shell.

Root cause: The requirement “ブラウザから直接ちゃんと対話” was interpreted as message submission instead of raw terminal interaction with the tmux-backed Orchestrator session.

Fix: Treat the xterm/WebSocket terminal as the primary Orchestrator interaction surface and remove the separate form from the UX.

Prevention: For Orchestrator session interaction, prefer a first-class Web shell with raw input/output behavior unless the user explicitly asks for an out-of-band message form.

### 2026-07-11 - Merge-ready evidence must reflect current PR review state

Tags: workflow/process, validation/verification, review, ci

Symptom: A worker reported merge-ready from an earlier hook exit 0 while the parent's fresh check still found Greptile description review pending and GitHub `reviewDecision=CHANGES_REQUESTED`.

Root cause: The handoff did not re-read the current PR review decision and unresolved review-thread state after later description and review updates.

Fix: Reconcile unresolved review threads and superseded reviews, update the PR description to the exact head, then rerun the blocking hook and PR JSON audit on that same head.

Prevention: Before every merge-ready handoff and orchestrator merge, require a fresh current-head `gh-review-hook` exit 0 plus non-draft, acceptable `reviewDecision`, CLEAN/not-behind state, successful checks, and exact local/remote head parity. Never reuse earlier hook evidence after any PR metadata, review, or head change.

### 2026-07-11 - Generation guards must include rendered target identity

Tags: assumptions/interpretation, review, validation/verification, realtime

Symptom: A generation-only terminal action guard allowed terminal B's newly rendered layout action to target terminal A before the hook's passive target-update effect ran.

Root cause: The review considered callbacks after `updateOptions`, but missed React's render → commit → child layout effect → parent passive effect ordering window.

Fix: Capture the rendered project/window identity with the generation and require all three to match the controller's current target before input or resize is sent.

Prevention: For target-switching realtime hooks, review and deterministically exercise actions both before and after passive effects. Generation tokens alone are insufficient unless they are synchronously invalidated during render; action guards must also bind the resource identity.
