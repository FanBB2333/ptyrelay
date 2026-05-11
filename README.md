# ptyrelay

> Turn any byte-stream shell session into a structured remote capability surface — file I/O, command execution, atomic uploads — without installing anything by default.

[![Release](https://img.shields.io/github/v/release/FanBB2333/ptyrelay?sort=semver)](https://github.com/FanBB2333/ptyrelay/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/FanBB2333/ptyrelay.svg)](https://pkg.go.dev/github.com/FanBB2333/ptyrelay)
[![Go Report Card](https://goreportcard.com/badge/github.com/FanBB2333/ptyrelay)](https://goreportcard.com/report/github.com/FanBB2333/ptyrelay)
[![Go Version](https://img.shields.io/github/go-mod/go-version/FanBB2333/ptyrelay)](go.mod)
[![License](https://img.shields.io/github/license/FanBB2333/ptyrelay)](LICENSE)

`ptyrelay` lets a local agent (Claude Code, an MCP client, a script) drive a
restricted-network remote host through a shell session you already have —
a `tmux` pane, a `ttyd`/`wetty` WebSocket bridge, a `docker exec -i`, a
`kubectl exec -i`, a plain `ssh -T`. Same client code, swap the transport.

## Highlights

- **Three transports, one stack.** `tmux` / WebSocket / subprocess all
  satisfy the same `Channel` interface — Session, Backend, and
  Bootstrap don't know which one is underneath.
- **Two backends, idempotency-aware routing.** `ShellBackend` works on
  any POSIX shell with zero install. `AgentBackend` runs a small Go
  binary on the remote (binary-safe I/O, separate stderr, classified
  errors, ~283× faster on repeated ops). `RouterBackend` picks per op
  and falls back transparently when safe.
- **Bootstrap, two ways.** Ship the agent binary from the local side
  (`--provider-dir`, or `-tags embedagents` for a self-contained CLI)
  or have the remote `curl`/`wget` it directly with sha256
  verification (`--from-url`).
- **CLI + MCP server.** `cmd/ptyrelay` for ad-hoc use,
  `cmd/ptyrelay-mcp` exposes nine tools (`read_file`, `write_file`,
  `run_command`, `list_dir`, `stat`, `mkdir`, `rename`, `remove`,
  `agent_info`) over stdio JSON-RPC for MCP clients.
- **Reliability extras.** Opt-in WebSocket keepalive ping/pong against
  half-open TCP, opt-in mid-session reconnect with explicit
  `ErrReconnected` signal, structured `log/slog` events across all
  three backends.

## Quickstart (CLI)

```sh
go install github.com/FanBB2333/ptyrelay/cmd/ptyrelay@latest

# Through an existing local tmux pane (the pane runs your ssh chain).
ptyrelay exec --tmux work:0.0 -- uname -a

# Through a ttyd / wetty / code-server WebSocket bridge.
ptyrelay get  --ws ws://host:8765/term /etc/hostname

# Straight into a container without ssh in the middle.
ptyrelay exec --exec "docker exec -i my-container bash" -- ps aux
ptyrelay get  --exec "kubectl exec -i -n prod api-0 -- bash" /var/log/app.log

# Auto-install the agent on first contact, then use it.
ptyrelay bootstrap --ws ws://host:8765/term --provider-dir dist/agents
ptyrelay agent-info --ws ws://host:8765/term
```

For every subcommand: exactly one of `--tmux`, `--ws`, or `--exec` is
required. Pass `--log-level=debug` to see structured probe / route
events on stderr.

## Quickstart (Library)

```go
package main

import (
    "context"
    "fmt"
    "log"

    "github.com/FanBB2333/ptyrelay/pkg/backend/shell"
    "github.com/FanBB2333/ptyrelay/pkg/channel/tmux"
    "github.com/FanBB2333/ptyrelay/pkg/session"
)

func main() {
    ctx := context.Background()

    // 1. Borrow a shell session that already exists in a tmux pane.
    ch, err := tmux.New(ctx, tmux.Options{Pane: "work:0.0"})
    if err != nil {
        log.Fatal(err)
    }
    defer ch.Close()

    // 2. Session adds sentinel framing (BEG/END markers + exit code)
    //    on top of the byte stream so each command's output is
    //    extractable from arbitrary shell noise.
    sess := session.New(ch, session.ShellBash)
    defer sess.Close()

    // 3. ShellBackend turns the Session into RemoteFS + RemoteExec.
    be := shell.New(sess)

    if err := be.Write(ctx, "/tmp/hello.txt",
        []byte("hi from ptyrelay\n"), 0o644); err != nil {
        log.Fatal(err)
    }
    data, _ := be.Read(ctx, "/tmp/hello.txt")
    fmt.Printf("read: %q\n", data)

    res, _ := be.Run(ctx, "uname -a", nil)
    fmt.Printf("uname: %s (exit=%d)\n", res.Stdout, res.ExitCode)
}
```

Need a tmux pane spun up on the fly? `tmux.InitSession` / `tmux.KillSession`
do the boilerplate.

## Architecture

```
       Client (CLI / MCP / your code)
                │
        ┌───────▼────────┐
        │    Backend     │  RouterBackend | AgentBackend | ShellBackend
        ├────────────────┤
        │    Session     │  sentinel framing, per-shell prelude, cancel chain
        ├────────────────┤
        │    Channel     │  tmux | websocket | subprocess
        └───────┬────────┘
                │
            Remote shell
```

- **Channel** — one ordered byte stream. Pluggable; pick or write your
  own (see `docs/TRANSPORTS.md`).
- **Session** — sentinel-framed RPC over the byte stream; isolates one
  command's output from shell noise.
- **Backend** — typed `RemoteFS` + `RemoteExec` ops, idempotency
  annotations.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the full picture,
[`docs/PROTOCOL.md`](docs/PROTOCOL.md) for the agent wire format,
[`docs/TRANSPORTS.md`](docs/TRANSPORTS.md) for per-transport notes and
recipes, and [`docs/SECURITY.md`](docs/SECURITY.md) for the threat model.

## Transports

| Transport       | BinarySafe | Separate stderr | Use when                                    |
|-----------------|:----------:|:---------------:|---------------------------------------------|
| `tmux`          | base64\*   | no              | You already have a tmux pane on a jumphost. |
| `websocket`     | yes        | no              | A `ttyd`/`wetty`/`code-server` WS bridge.   |
| `subprocess`    | yes        | no              | `docker exec`, `kubectl exec`, `ssh -T`.    |

\* tmux's PTY layer corrupts raw NUL/high bytes; AgentBackend stages
binary payloads as base64 to round-trip safely.

## MCP

```sh
go install github.com/FanBB2333/ptyrelay/cmd/ptyrelay-mcp@latest
```

Configure transport via env (`PTYRELAY_TRANSPORT=ws|tmux|exec`,
`PTYRELAY_WS_URL=…`, etc.) and point your MCP client at the binary.
Tools: `read_file`, `write_file`, `run_command`, `list_dir`, `stat`,
`mkdir`, `rename`, `remove`, `agent_info`.

## Build / Test

```sh
go build ./...

# Dev loop — skips multi-MB PTY upload integration tests, ~50s.
go test -short -race ./...

# Full integration (Bootstrap, e2e_FullStack, SessionOverWebSocket).
# Multiple PTY-bound packages contend under -race, so serialize.
go test -p 1 ./...

# Self-contained CLI with embedded agent matrix.
scripts/build-agents.sh
cp dist/agents/* cmd/ptyrelay/agents/
go build -tags embedagents -o ptyrelay ./cmd/ptyrelay
```

`bash` and `tmux` are needed for the matching integration tests;
the suites auto-skip when either is missing.

## License

MIT — see [LICENSE](LICENSE).
