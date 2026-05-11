# Transports

ptyrelay decouples the byte-stream transport from the protocol that
rides on top of it. Anything that satisfies `pkg/channel.Channel` plugs
into Session + Backend without code changes elsewhere — see the
`TestE2E_SessionOverWebSocket` test for the abstraction-closure proof.

This document covers the two transports shipped in v0.3.0 and the
trade-offs picking between them.

## At a glance

| Property            | tmux                       | WebSocket                          | subprocess                          |
|---------------------|----------------------------|------------------------------------|-------------------------------------|
| `BinarySafe`        | false                      | **true**                           | **true**                            |
| `MaxWriteChunk`     | 32 KiB                     | unlimited                          | unlimited                           |
| `SeparateStderr`    | false                      | false                              | false                               |
| `ScrollbackLimited` | false                      | false                              | false                               |
| Concurrent reads    | no                         | no                                 | no                                  |
| Needs remote tool   | `tmux`                     | any WS bridge                      | any command speaking stdio          |
| Resize support      | yes                        | hook-defined                       | no                                  |
| Typical use case    | SSH+tmux pane on jumphost  | code-server / cloud shell / ttyd   | `docker exec` / `kubectl exec` / `ssh -T` |

## TmuxChannel — `pkg/channel/tmux`

### How it works

- **Write**: `tmux send-keys -l -t <pane> -- <bytes>`. The `-l` flag
  forces literal mode (every byte goes to the pane's PTY input rather
  than being parsed as a tmux key name), so `\x03` actually sends
  Ctrl-C. Payloads are chunked at 32 KiB because `send-keys` truncates
  silently above that on some builds.
- **Read**: `tmux pipe-pane -o -t <pane> 'cat >> <logfile>'` starts a
  detached writer; ptyrelay tails the logfile from the local side.
  `pipe-pane` is preferred over `capture-pane` because the former
  streams every byte the pane produced as it produced it, while the
  latter reads a finite, ANSI-flattened scrollback buffer.
- **Resize**: `tmux resize-window -t <pane> -x <cols> -y <rows>`.
- **Close**: tear down `pipe-pane` and (when the logfile is owned by
  ptyrelay) delete the file.

### Caveats

- **BinarySafe = false**. `send-keys -l` does pass arbitrary bytes
  through, but the pane's PTY is in cooked mode by default — NUL,
  certain control sequences, and CR/LF translation can reinterpret
  payload bytes. The Session layer's per-shell prelude (`stty -echo
  -onlcr -icanon`) tames most of this, and binary payloads should
  travel inside Session frames (base64 in shell mode, length-prefixed
  in agent REPL mode) rather than as raw `Channel.Write` bytes.
- **User interference**. Anything a human types into the same pane
  becomes input the Channel sees on the read side. Reserve a tmux
  window/session for ptyrelay; the README warns about this.
- **macOS PTY MAX_INPUT**. Large `send-keys` to a pane whose input
  buffer is near the OS limit (≈1 KiB on macOS) will drop bytes.
  ShellBackend's chunked-base64 write logic and AgentBackend's
  staged-tempfile path both work around this, but it's why
  `MaxWriteChunk = 32 KiB` is paired with the `Caps.MaxWriteChunk`
  honoring everywhere upstream.

### When to pick tmux

The default for SSH + jumphost flows where you already have a tmux
session running. No extra services to deploy on the remote — just
tmux. Latency is good (single-digit ms per send-keys), throughput is
fine for kilobyte-scale payloads.

## WebSocketChannel — `pkg/channel/websocket`

### How it works

- **Dial**: gorilla/websocket `DialContext` with a configurable
  handshake timeout and optional headers (auth tokens, Origin, etc.).
- **Write**: one WebSocket frame per `Write` call. Default is a
  binary frame with the payload bytes verbatim; the `Options.Encode`
  hook can wrap or transform first.
- **Read**: a single goroutine pumps received frames into a shared
  buffer, and `Read` drains it. Default is to treat any frame
  (Text or Binary) as raw bytes; `Options.Decode` can strip an
  envelope.
- **Resize**: `Options.EncodeResize`, if set, builds and sends a
  resize control frame. nil means Resize is a no-op.
- **Close**: send a polite WebSocket close frame, then drop the
  underlying connection. Safe to call more than once.

### Pluggable envelopes

The default raw-bytes-in-binary-frames behavior matches the simplest
stdio-over-WS bridges (`socat ws ↔ bash`, hand-rolled proxies). Real
terminal servers usually wrap their stream:

#### ttyd

ttyd prepends one byte per message: `'0'` for stdin/stdout payload,
`'1'` for resize. Adapter:

```go
ch, _ := websocket.Dial(ctx, websocket.Options{
    URL: "ws://host/ws",
    Encode: func(b []byte) ([]byte, int, error) {
        return append([]byte{'0'}, b...), websocket.TextMessage, nil
    },
    Decode: func(mt int, b []byte) ([]byte, error) {
        if len(b) < 1 || b[0] != '0' {
            return nil, nil // ignore non-stdout frames
        }
        return b[1:], nil
    },
    EncodeResize: func(cols, rows uint16) ([]byte, int, error) {
        // ttyd resize: '1' + JSON `{"columns": …, "rows": …}`
        body := fmt.Sprintf(`1{"columns":%d,"rows":%d}`, cols, rows)
        return []byte(body), websocket.TextMessage, nil
    },
})
```

The unit test `TestChannel_EncodeDecodeHooks` exercises this
envelope pattern end-to-end.

#### Generic stdio bridge (recommended for jumphosts)

If you control the server side, a 50-line WebSocket-to-bash bridge
gives you raw binary frames and full Session/Backend compatibility.
The bridge used by ptyrelay's own tests (`startBashOverWS` in
`pkg/channel/websocket/integration_test.go`) is exactly that pattern.

#### code-local / remoteterminal

The code-local protocol wraps frames in a JSON envelope. ptyrelay's
v0.3.0 ships the generic Channel; a thin adapter package
(`pkg/channel/websocket/codelocal`) is a natural follow-up once the
upstream wire format is pinned down.

### Caveats

- **No SeparateStderr**. WS is binary-safe but the bridge usually
  multiplexes stdout+stderr into one byte stream (because the
  remote side is a single PTY). Use AgentBackend if you need stderr
  separation — it carries its own envelope.
- **No back-pressure visibility**. gorilla doesn't expose the
  per-message TCP buffer state; `Write` returns once the bytes are
  in the OS socket buffer, not after delivery. The Session layer's
  ack-by-sentinel mechanism still works, but timing-sensitive
  callers should think in terms of round-trips rather than single
  writes.
- **Reconnect is out of scope**. v0.3.0's Channel handles a single
  connection. Reconnection on transient failure belongs in a higher
  layer (or a future option).

### When to pick WebSocket

- Browser-resident terminals (code-server, ttyd, GitHub Codespaces).
- Cloud shells with `/exec` WS endpoints.
- Any environment where SSH+tmux isn't an option but a WebSocket
  pipe to a shell exists.
- When you want binary safety end-to-end (large file pushes via
  AgentBackend REPL stay binary all the way down).

## SubprocessChannel — `pkg/channel/subprocess`

### How it works

The simplest possible adapter: launch a local command, treat its
stdin/stdout as the Channel byte stream. Anything that already exposes
"stdin in, stdout out" — `docker exec -i`, `kubectl exec -i`,
`lxc exec`, `podman exec`, `ssh -T` — becomes a Channel without
writing a transport.

```go
ch, _ := subprocess.Start(ctx, subprocess.Options{
    Command: []string{"docker", "exec", "-i", "my-container", "bash"},
})

// or kubectl:
ch, _ := subprocess.Start(ctx, subprocess.Options{
    Command: []string{"kubectl", "exec", "-i", "-n", "prod", "api-0", "--", "bash"},
})

// or plain ssh:
ch, _ := subprocess.Start(ctx, subprocess.Options{
    Command: []string{"ssh", "-T", "user@host", "bash"},
})
```

### Caveats

- **No PTY.** The child runs with regular pipes; programs that
  detect "am I a terminal?" (e.g. `vim`, full-screen TUIs) won't
  enable terminal mode. For the framed-command use case ptyrelay is
  built for, this is fine — Session's prelude turns echo off
  *because* it's running over a PTY, and pipes don't echo anyway.
- **No Resize.** Without a PTY there's no geometry to update. If
  you need that, layer this Channel inside a creack/pty wrapper.
- **stderr is dropped.** The Channel exposes only stdout. Most
  `<runner> exec -i` invocations merge stderr into stdout anyway;
  if you need separation, route stderr to a log file via your own
  wrapper command.

### When to pick subprocess

- Local container debugging (`docker exec`).
- Kubernetes troubleshooting (`kubectl exec`).
- Air-gapped jumphost where you can `ssh -T` from your workstation
  and don't want a tmux session for ptyrelay's exclusive use.

## Adding a new transport

The contract is `pkg/channel.Channel`:

```go
type Channel interface {
    io.Reader
    io.Writer
    Resize(ctx context.Context, cols, rows uint16) error
    Close() error
    Caps() Caps
}
```

Implementation checklist:

1. Decide `Caps`. The two values that matter most:
   - `BinarySafe`: are NUL and `\x03` etc. delivered unmodified?
   - `MaxWriteChunk`: largest atomic Write the transport accepts.
2. Read should block until at least one byte is available.
3. `Close` must be idempotent and unblock any pending `Read`.
4. Add a `TestE2E_SessionOver<X>` that wires your Channel to a real
   bash subprocess and runs `shell.Backend.Probe / Run / Write /
   Read` against it. If the existing Session/Backend stack passes
   unchanged, the abstraction holds.

Candidate transports already in the ideas pile (TODOs.md):

- `KubectlExecChannel` — `kubectl exec -it pod -- bash`. Same shape
  as the bash-over-WS bridge.
- `DockerExecChannel` — `docker exec -it container bash`.
- `SerialChannel` — direct UART, for device/embedded debugging.
