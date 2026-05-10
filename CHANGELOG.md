# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- Project skeleton: directory layout, Go module, CI, lint, editorconfig.
- Core interfaces (`Channel`, `Session`, `Backend`, `RemoteFS`, `RemoteExec`) with idempotency annotations.
- Sentinel-framed `Session` with per-shell prelude (bash/zsh/dash) and a Ctrl-C → Ctrl-\ cancellation escalation.
- `ShellBackend` with atomic writes, sha256 verification, streaming read/write, and a busybox-aware compatibility layer.
- `TmuxChannel` transport (send-keys + pipe-pane) with a `tmux-init` helper.
- README quickstart and `docs/ARCHITECTURE.md`.
