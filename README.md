# ptyrelay

> Relay your shell over any pty.

让 Claude Code 等本地 agent 通过既有 pty 通道（tmux+SSH、code-local WebSocket 等）操控受限网络环境下的远端服务器。

## Status

v0.1.0 — Shell-only MVP. 详见 [TODOs.md](TODOs.md) 与 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## Build

```sh
go build ./...
```

## Test

```sh
go test ./...
```
