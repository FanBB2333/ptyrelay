# ptyrelay

> Relay your shell over any pty.

让 Claude Code 等本地 agent 通过既有 pty 通道（tmux+SSH、code-local
WebSocket 等）操控受限网络环境下的远端服务器——把"只是一个字节流"的 shell
session 变成一个结构化的远端能力面：文件读写、命令执行。

## Status

**v0.2.0 — Agent + Router.** 既有 ShellBackend（什么都不预装就能跑），也有
AgentBackend（远端跑一个 Go 二进制走 JSON RPC，binary-safe、stderr
独立、错误分类）。RouterBackend 按 op 幂等性自动选路：
agent 健康时走 RPC，agent 缺失或 ReadOnly/Idempotent 失败时透明回落
到 shell；NonIdempotent 失败上抛由调用方决策。Bootstrap 会自动把 agent
二进制 atomic-write 到远端（sha256 校验 + 平台探测）。

REPL 模式（agent 长寿进程，单 op 延迟 < 1ms）和 WebSocket transport
留到 v0.3.0。详见 [TODOs.md](TODOs.md)、
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)、
[docs/PROTOCOL.md](docs/PROTOCOL.md)、
[docs/SECURITY.md](docs/SECURITY.md)。

## Quickstart

```sh
go get github.com/FanBB2333/ptyrelay@latest
```

通过现有 tmux pane 操作远端：

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

	// 1. 在某个 tmux pane 里准备好你想"借用"的 shell 通道。
	//    常见做法：tmux 内 ssh 到跳板机，再 ssh 到目标。
	ch, err := tmux.New(ctx, tmux.Options{Pane: "my-session:0.0"})
	if err != nil {
		log.Fatal(err)
	}
	defer ch.Close()

	// 2. Session 在 Channel 之上加 sentinel framing：每条命令都被
	//    BEG/END 包起来,这样 ptyrelay 能从混杂的 shell 输出里准确
	//    捞出本次命令的 stdout 和 exit code。
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()

	// 3. ShellBackend 把 Session 包装成 RemoteFS + RemoteExec。
	be := shell.New(sess)

	// 文件读写
	if err := be.Write(ctx, "/tmp/hello.txt", []byte("hi from ptyrelay\n"), 0o644); err != nil {
		log.Fatal(err)
	}
	data, err := be.Read(ctx, "/tmp/hello.txt")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("read: %q\n", data)

	// 命令执行
	res, err := be.Run(ctx, "uname -a", nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("uname: %s (exit=%d)\n", res.Stdout, res.ExitCode)
}
```

需要先有 tmux pane？用内置的 `InitSession` helper 起一个：

```go
opts, _ := tmux.InitSession(ctx, tmux.InitOptions{
	SessionName: "ptyrelay-demo",
	Command:     "ssh user@jumphost",
})
defer tmux.KillSession(ctx, tmux.InitOptions{SessionName: "ptyrelay-demo"})

ch, _ := tmux.New(ctx, opts)
// ...同上
```

## Build / Test

```sh
go build ./...
go test ./...
```

测试矩阵需要 `bash` + `tmux`，缺哪个对应的集成测试会自动 skip。

## License

MIT — see [LICENSE](LICENSE).
