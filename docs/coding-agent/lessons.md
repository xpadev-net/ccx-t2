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
