# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added (v0.2.0)
- `pkg/proto`: agent wire protocol (typed Request/Response, op constants,
  ErrKind taxonomy, length-prefixed and line-delimited codecs).
- `cmd/ptyrelay-agent`: remote binary supporting one-shot and REPL modes;
  ops ping/read/write/stat/lstat/list/remove/rename/mkdir_all/run/bye.
- `pkg/backend/agent`: AgentBackend with two transports:
  - `ModeOneShot` (default): one agent process per op; base64-staged
    requests via tempfile to survive PTY MAX_INPUT.
  - `ModeREPL`: long-lived agent over `Session.Pipe`; serialized
    request/response multiplexed through one process. ~5000× faster
    than one-shot on repeated ops, gated by an internal warmup ping
    that confirms the agent is reading before any user payload.
- `pkg/session`: real `Session.Pipe` (replacing the v0.1.0 stub) with
  a streaming sentinel parser that delivers bytes line-by-line, plus
  channel-write serialization so pipe-stdin and the cancel chain
  can't interleave.
- `pkg/bootstrap`: agent install over ShellBackend (uname-based platform
  probe, FileProvider/EmbedProvider, atomic write + sha256 verify).
- `pkg/backend/router`: RouterBackend with idempotency-aware fallback
  (ReadOnly/Idempotent silently retry through shell; NonIdempotent
  surface errors to caller).
- Docs: `docs/PROTOCOL.md` (canonical wire spec), `docs/SECURITY.md`
  (threat model + mitigations).

### Added (v0.1.0)
- Project skeleton: directory layout, Go module, lint, editorconfig.
- Core interfaces (`Channel`, `Session`, `Backend`, `RemoteFS`, `RemoteExec`) with idempotency annotations.
- Sentinel-framed `Session` with per-shell prelude (bash/zsh/dash) and a Ctrl-C → Ctrl-\ cancellation escalation.
- `ShellBackend` with atomic writes, sha256 verification, streaming read/write, and a busybox-aware compatibility layer.
- `TmuxChannel` transport (send-keys + pipe-pane) with a `tmux-init` helper.
- README quickstart and `docs/ARCHITECTURE.md`.
