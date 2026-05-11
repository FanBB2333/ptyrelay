# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `bootstrap.Options.FromURL`: callback that returns `(url, sha256)`
  for a given `(osName, arch)`. When set, the remote downloads the
  agent via `curl` / `wget` instead of taking a multi-MB upload from
  the local side. URLs ending in `.gz` are gunzipped on the remote;
  empty sha256 disables verification. Falls back to `Provider` when
  `FromURL` is nil.
- `cmd/ptyrelay bootstrap`: new flags `--from-url <template>` and
  `--from-url-sha256 <hex>`. The template supports `{os}` / `{arch}`
  substitution, so a release layout like
  `https://host/ptyrelay-agent-{os}-{arch}.gz` covers the whole matrix.
- `scripts/build-agents.sh`: cross-compile build script that emits
  the agent for 8 platforms (linux/{amd64,arm64,386,arm},
  darwin/{amd64,arm64}, freebsd/{amd64,arm64}) into
  `dist/agents/<os>-<arch>`, the layout `FileProvider` /
  `EmbedProvider` expect. `--gzip` additionally emits `.gz` siblings
  ready for `--from-url`.

- `cmd/ptyrelay-mcp`: four new tools — `mkdir`, `rename`, `remove`,
  `agent_info`. The first three close the gap between MCP and the
  Backend interface; `agent_info` reports which backend
  (`shell` / `router(agent+shell)` / `router(shell-only)`), transport,
  and agent path so an LLM operator can reason about Caps before
  issuing expensive ops.
- `cmd/ptyrelay -tags embedagents`: build-tag-gated `//go:embed` of
  `cmd/ptyrelay/agents/` that wires `bootstrap.EmbedProvider` into the
  CLI. Default builds stay slim and reject `bootstrap` /  `--install`
  without `--provider-dir` or `--from-url`. With the tag set, the
  binary self-contains its agent matrix and can install on a fresh
  remote with zero side-channels. Workflow:
  `scripts/build-agents.sh && cp dist/agents/* cmd/ptyrelay/agents/ &&
  go build -tags embedagents ./cmd/ptyrelay`.

### Fixed
- `pkg/bootstrap/fetch.go`: stage the fetch script to a remote tempfile
  via `shell.Backend.Write` and invoke as `sh <tmpfile>`, instead of
  passing the script inline through `sh -c '<~1KB script>'`. The inline
  path hung the framed Session indefinitely in a narrow 900–1500-byte
  band on macOS bash 3.2 — single-Write payloads of that size lost
  synchronization with the PTY input buffer and the END marker never
  arrived. Staging keeps the framed command line constant (~120 bytes)
  regardless of script size, so the in-band-buffer interaction can't
  trigger. Repro: `TestBootstrap_FromURL` / `_SHA256Mismatch` hung 67 s
  before this; ~100 ms after.
- Multi-line shell scripts containing `exit N` no longer kill the
  framed Session. `pkg/bootstrap/fetch.go` runs the inner script in
  a child `sh -c '…'` so non-zero exits propagate as the command's
  exit code instead of taking down the parent bash.

### Added (continued)
- `websocket.Options.DialRetries` + `DialBackoff`: retry transient
  transport failures (refused, timeout) with exponential backoff.
  HTTP-level upgrade failures (4xx/5xx returned via
  `gws.ErrBadHandshake`) are treated as terminal and surface
  immediately — silent retry there would mask config bugs.
- `pkg/channel/subprocess`: third Channel implementation. Launches
  a local command and uses its stdio as the byte stream. Covers
  `docker exec -i`, `kubectl exec -i`, `lxc exec`, `ssh -T`, and
  any other "stdio-in / stdio-out" runner in a single ~150-line
  package. Default `BinarySafe=true`, unlimited `MaxWriteChunk`,
  no Resize (use a PTY wrapper if you need geometry).
- `docs/TRANSPORTS.md`: subprocess section + recipes.
- `cmd/ptyrelay --exec "<argv>"`: subprocess transport at the CLI.
  Argv is whitespace-split — wrap complex commands in `bash -c '…'`.
  Exclusivity check covers all three transports.
- `cmd/ptyrelay-mcp`: same via `PTYRELAY_TRANSPORT=exec` +
  `PTYRELAY_EXEC="<argv>"`.
- `websocket.Options.Reconnect` + `MaxReconnects` +
  `ReconnectBackoff`: opt-in mid-session reconnect. When the
  underlying TCP drops the Channel re-Dials, discards any
  buffered bytes from the dead connection, and signals the
  pending Read once with the new `websocket.ErrReconnected`
  sentinel. Subsequent Reads/Writes run against the fresh
  connection. Reconnect is *not* magic: a new TCP gets a fresh
  remote process with fresh state, so higher layers that own
  framing/sentinel state (FramedSession, AgentBackend REPL) must
  treat `ErrReconnected` as "rebuild from scratch."
- `websocket.Options.PingInterval` + `PongTimeout`: WebSocket-level
  keepalive. When `PingInterval > 0`, the Channel sends `PingMessage`
  every interval and extends the read deadline on every Pong. Half-open
  TCP no longer hangs Read forever — the connection surfaces an error
  after ~`PongTimeout` (default 3×PingInterval). Recommended starting
  point: 30 s. Default is opt-out (0).
- Structured logging (`log/slog`): `shell.WithLogger`,
  `agent.WithLogger`, `router.WithLogger`. Default is silent (no-op
  handler) — opt-in only. Events emitted: `probe.start/done`,
  `agent.healthy/unhealthy`, `route` (which backend served each op),
  `agent.error.no_fallback` for the NonIdempotent-can't-fall-back
  case. CLI exposes `--log-level debug|info|warn|error` (events go
  to stderr); MCP follows the host's convention and stays silent.

## [0.3.0] — 2026-05-11

### Added
- `pkg/channel/websocket`: generic [channel.Channel] backed by a single
  WebSocket connection (gorilla/websocket). Defaults to "raw bytes in
  binary frames"; ttyd / code-local / wetty-style envelopes are pluggable
  via `Options.Encode` / `Decode` / `EncodeResize` hooks. Caps reports
  `BinarySafe=true` and unbounded `MaxWriteChunk`.
- E2E test (`integration_test.go`) wires a bash-over-WebSocket bridge
  through Session + ShellBackend without any Backend-level changes,
  proving the v0.3.0 promise: swap the Channel, keep the rest.
- `cmd/ptyrelay`: real CLI (replaces the v0.1.0 placeholder). Subcommands
  `exec / get / put / stat / ls / bootstrap / agent-info`, transport
  picked per-invocation via `--tmux <pane>` or `--ws <url>`, optional
  `--install` auto-bootstrap, `--no-agent` to force shell-only. Each
  subcommand exits with the remote command's exit code where applicable.
- `cmd/ptyrelay-mcp`: Model Context Protocol server over stdio JSON-RPC.
  Hand-rolled (no external SDK) supports `initialize`, `tools/list`,
  `tools/call`. Exposes `read_file`, `write_file`, `run_command`,
  `list_dir`, `stat` tools. Transport configured via env (`PTYRELAY_*`),
  so MCP clients (e.g. Claude Code) only need to launch the binary.
- `pkg/backend/agent/bench_test.go`: REPL vs one-shot Probe benchmark.
  On Apple M1 Pro, darwin/arm64: one-shot 157.5 ms/op,
  REPL 0.555 ms/op — **~283× speedup** for repeated ops. Run with
  `go test -bench=Probe -run='^$' ./pkg/backend/agent/`.
- `docs/TRANSPORTS.md`: per-transport notes (tmux vs WebSocket trade-offs,
  Caps cheat sheet, ttyd / generic-bridge / code-local adapter recipes,
  checklist for adding a new Channel).

## [0.2.0]

### Added
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

## [0.1.0]

### Added
- Project skeleton: directory layout, Go module, lint, editorconfig.
- Core interfaces (`Channel`, `Session`, `Backend`, `RemoteFS`, `RemoteExec`) with idempotency annotations.
- Sentinel-framed `Session` with per-shell prelude (bash/zsh/dash) and a Ctrl-C → Ctrl-\ cancellation escalation.
- `ShellBackend` with atomic writes, sha256 verification, streaming read/write, and a busybox-aware compatibility layer.
- `TmuxChannel` transport (send-keys + pipe-pane) with a `tmux-init` helper.
- README quickstart and `docs/ARCHITECTURE.md`.
