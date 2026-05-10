# ptyrelay

> Relay your shell over any pty.

让 Claude Code 等本地 agent 通过既有 pty 通道（tmux+SSH、code-local
WebSocket 等）操控受限网络环境下的远端服务器——把"只是一个字节流"的 shell
session 变成一个结构化的远端能力面：文件读写、命令执行。

## Status

**v0.1.0 — Shell-only MVP.** 只用 POSIX shell 命令拼出全部远端能力，
不需要在远端预装任何东西。性能不算好（每个 op 都是一次 shell 往返），
但抽象边界已经成型，准备在 v0.2.0 加上长寿 agent 进程把延迟降到
毫秒级。详见 [TODOs.md](TODOs.md) 的发布节奏与
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) 的分层设计。

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
