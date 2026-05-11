# ptyrelay Architecture

This document explains the layering, the choices behind each layer, and where new transports/backends plug in. For the project rationale see [README.md](../README.md); for the milestone-by-milestone plan see [TODOs.md](../TODOs.md).

## Layers

```
        Caller (CLI / MCP / library code)
                │
        ┌───────┴───────┐
        │    Backend    │   RemoteFS + RemoteExec
        └───────┬───────┘
                │
        ┌───────┴───────┐
        │    Session    │   sentinel framing + nonce + cancel chain
        └───────┬───────┘
                │
        ┌───────┴───────┐
        │    Channel    │   bytes in / bytes out + Caps
        └───────┬───────┘
       ┌────────┼─────────────┐
       │        │             │
  TmuxChannel  WebSocketChannel  SubprocessChannel
```

Each layer has one responsibility and is replaceable independently of the layers above and below it.

### Channel — `pkg/channel`

A `Channel` is a bidirectional, ordered byte pipe with a `Caps` descriptor. It is the only layer that knows about the underlying transport (tmux pane, WebSocket bridge, `docker exec`/`kubectl exec` stdio, etc.).

`Caps` exposes the transport's idiosyncrasies — `BinarySafe`, `MaxWriteChunk`, `ScrollbackLimited` — so higher layers can adapt without per-transport branching. For example, a binary-safe channel skips base64 wrapping; a transport with a tight `MaxWriteChunk` triggers the upper layer's chunked-write path.

The Channel layer does *not* worry about framing, prompts, or shell semantics. It handles bytes.

### Session — `pkg/session`

A `Session` turns a noisy, continuous PTY byte stream into request/response RPC. It does this with **sentinel framing**:

```
__PR_N=<nonce>; printf '\n__PR_BEG_'$__PR_N'__\n'; { <user cmd>
}; __PR_RC=$?; printf '\n__PR_END_'$__PR_N'__:%d\n' "$__PR_RC"
```

Two pieces of design earn their keep here:

1. **The nonce is inserted via a shell variable, not as a literal.** When PTY echo sends our wrapper command back to us, the echoed bytes contain the literal `$__PR_N`. The actual marker bytes appear only after the shell expands the variable at run time. The parser scans for the *substituted* form, so echo can never collide with the real markers — we don't need to detect or filter the echo.

2. **A single session-scoped reader goroutine.** Sequential `RunFramed` calls share one reader; per-call goroutines would race for bytes still in flight on the previous Read syscall. The first-cut design had per-call goroutines and a tail-end byte race surfaced almost immediately on macOS bash 3.2.

Other Session-layer responsibilities:

- **Per-shell prelude** — bash/zsh/dash/sh-aware: `stty -echo -onlcr -icanon`, `LC_ALL=C`, `PS1=''`, history off. Runs lazily on first call. Subsequent commands then live in a deterministic, low-noise environment.
- **Cancellation escalation** — `ctx.Done` → write `\x03` (Ctrl-C) → wait `softCancelGrace` → write `\x1c` (SIGQUIT) → wait `hardCancelGrace` → declare the session dead. Each phase has its own hard timeout so a `vim` swallowing SIGINT can't lock the session.
- **ANSI/CR strip** for output — needed on real PTYs even with `-onlcr`, because the prelude itself runs before its own stty takes effect.

The Session contract serializes calls (single mutex). The remote shell is single-threaded, so this matches reality.

### Backend — `pkg/backend`

The `Backend` interface composes `RemoteFS` (file ops) and `RemoteExec` (command execution). It is what callers actually consume — the layers below are plumbing.

Each op carries an idempotency class:

| Op | Class | Auto-retry on transient error | Auto-fallback when an agent dies |
|---|---|---|---|
| ping/read/stat/lstat/list/open_read | ReadOnly | yes | yes |
| mkdir/rename/write/open_write | Idempotent | yes (atomic write makes redo safe) | yes |
| remove/run | NonIdempotent | no | no |

`Op.Class()` returns this in code; `RouterBackend` (since v0.2.0) consults it when deciding whether a Shell fallback is safe after an agent op fails — `ReadOnly` and `Idempotent` retry silently through Shell, `NonIdempotent` surface the error to the caller.

#### ShellBackend — `pkg/backend/shell`

Composes shell commands to fulfill the Backend contract:

- **Read**: `cat <path> | base64`, then base64-decode locally. A `Stat` runs first so files larger than `MaxShellFileSize` (default 4 MiB) fail fast with `ErrTooLarge`.
- **Write**: payload is base64-encoded and sliced into ~32 KiB chunks (so the shell command line stays under typical ARG_MAX). The first chunk creates a tempfile (`<path>.tmp.<nonce>`); subsequent chunks append. After all bytes land, sha256 is computed remotely and compared to the local digest. Only then does `chmod` + `mv` move the tempfile into place. Any failure cleans up the tempfile.
- **Stat / Lstat**: the same parser handles GNU `stat -c '%s|%Y|%a|%F'` and BSD `stat -f '%z|%m|%Lp|%HT'`; the probe selects the format string. Symlink targets are resolved via `readlink` only when Lstat sees `IsSymlink`.
- **List**: `find -mindepth 1 -maxdepth 1 -print0 | xargs -0 stat …`. The null separator means filenames with spaces, newlines, or shell metacharacters are handled correctly.
- **Run**: passes through to the Session. Stdin (if any) is delivered via a here-doc whose delimiter is nonce-derived and rejected if the user payload contains it.

**Probe** runs once per backend, in a single shell invocation, and caches:
- OS (`uname -s`)
- whether `base64`, `gzip`, `sha256sum`/`shasum` exist
- whether `base64` accepts `-w0` (GNU does, BSD doesn't)
- GNU vs BSD `stat`
- whether `find -printf` works

Without `base64`, ShellBackend refuses to construct — bytes can't be encoded.

## Why these splits

The **Channel ↔ Session** seam is what makes "borrow any existing shell channel" work. v0.3.x ships three Channels (`tmux`, `websocket`, `subprocess`) and the Session/Backend layers above them are identical for all three. The acceptance test for that seam being correct: integration tests run the *same* `ShellBackend` test suite over each Channel and they all pass.

The **Session ↔ Backend** seam is what makes the upgrade from "shell commands" to "agent RPC" non-disruptive. `ShellBackend` and `AgentBackend` both implement `RemoteFS + RemoteExec`. The op-class metadata on the interface lets `RouterBackend` route between them per-call without callers re-thinking error handling.

## Handling PTY echo

This is the question the project's design hinges on, and the answer above is short enough to easily miss. Restating it explicitly:

- We send a wrapper command. In a PTY in cooked mode, the kernel's line discipline echoes it back to our master fd. So the byte stream we read contains both *what we sent* (echoed) and *what the shell emitted* (the actual command output).
- The wrapper carries the run-time nonce in shell variable form (`'\n__PR_BEG_'$__PR_N'__\n'`). The bytes that get echoed contain the literal `$__PR_N` because PTY echo doesn't expand shell variables. The shell then runs `printf` and writes the *substituted* form (`__PR_BEG_<nonce>__`) to its stdout, which the PTY relays back to us.
- The parser scans for `__PR_BEG_<nonce>__`. The echo doesn't contain that. The substituted output does. There is no ambiguity, no need to track "which line is the echo" — the bytes are distinct.

If we used a literal nonce in the wrapper (i.e. `printf '__PR_BEG_<nonce>__'`), the echo would carry the same bytes as the real output, and the parser would lock onto the first occurrence — typically the echo, which means we'd start "capturing output" before the real output arrived. That is the bug this whole approach prevents.

## Layout

```
.
├── cmd/
│   ├── ptyrelay/         # local CLI (exec / get / put / stat / ls / bootstrap / agent-info)
│   ├── ptyrelay-agent/   # remote binary (one-shot + REPL modes)
│   └── ptyrelay-mcp/     # MCP stdio server exposing nine tools
├── pkg/
│   ├── channel/          # Channel interface + Caps
│   │   ├── tmux/         # TmuxChannel
│   │   ├── websocket/    # WebSocketChannel (gorilla/websocket)
│   │   └── subprocess/   # SubprocessChannel (docker/kubectl/ssh stdio)
│   ├── session/          # FramedSession (sentinel framing, cancel chain)
│   ├── backend/
│   │   ├── *.go          # interfaces + op-class table
│   │   ├── shell/        # ShellBackend (zero-install)
│   │   ├── agent/        # AgentBackend (JSON-RPC over Session)
│   │   └── router/       # RouterBackend (idempotency-aware fallback)
│   ├── bootstrap/        # agent install (Provider + FromURL)
│   └── proto/            # agent wire protocol (typed Request/Response)
├── internal/
│   ├── shellquote/       # POSIX single-quote escape
│   └── testpty/          # creack/pty-backed Channel for tests
└── scripts/
    └── build-agents.sh   # cross-compile matrix for bootstrap providers
```
