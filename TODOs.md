# ptyrelay — Project TODO

> Relay your shell over any pty. 让 Claude Code 等本地 agent 通过既有 pty 通道（tmux+SSH、code-local WebSocket 等）操控受限网络环境下的远端服务器。

---

## 背景与目标

很多生产服务器位于跳板机后或无外部网络环境，本地的 Claude Code 无法直连。已有的进入通道（一个 SSH session、一个 code-server WebSocket）虽然只是字节流，但理论上足以承载结构化 RPC。ptyrelay 的目标是把这个观察工程化：

- **统一的远端能力抽象**：文件读写、命令执行，对调用方完全屏蔽底层 transport
- **多 transport 并存**：SSH+tmux、code-local WebSocket，未来可扩展（kubectl exec、docker exec、串口等）
- **双 backend 自动降级**：远端有 agent 二进制时走 RPC，否则用 shell 命令拼出能力，并把 shell 通道作为 agent 的 bootstrap 路径
- **零远端预装**：用户不需要在远端事先安装任何东西

---

## 架构

```
        Claude Code / 调用方
                │
        ┌───────┴───────┐
        │    Backend    │   RemoteFS + RemoteExec    （transport-agnostic）
        └───────┬───────┘
                │
        ┌───────┴───────┐
        │    Session    │   sentinel framing + nonce + timeout
        └───────┬───────┘
                │
        ┌───────┴───────┐
        │    Channel    │   bytes in / bytes out + Caps
        └───────┬───────┘
       ┌────────┴─────────┐
       │                  │
  TmuxChannel       WebSocketChannel
  (本机 tmux         (code-local
   send-keys/         remoteterminal
   pipe-pane)         channel)
```

Backend 有两个实现：`AgentBackend`（RPC 走远端 Go 二进制）和 `ShellBackend`（拼 shell 命令）。`RouterBackend` 在构造时探测 agent，按 op 幂等性路由：agent 健康时走 RPC，agent 缺失或崩溃时 ReadOnly/Idempotent op 自动降级到 shell 并触发 re-bootstrap，NonIdempotent op 上抛错误由调用方决策。Shell 同时也是 agent 的 bootstrap 通道。

---

## Milestones

### M0 — 仓库与工程基线

- [ ] `go mod init github.com/FanBB2333/ptyrelay`
- [ ] 目录布局
  - [ ] `cmd/ptyrelay/` — 本地 CLI / library 入口
  - [ ] `cmd/ptyrelay-agent/` — 远端二进制
  - [ ] `pkg/channel/` — Channel 接口与实现
  - [ ] `pkg/session/` — sentinel framing
  - [ ] `pkg/backend/` — Agent / Shell / Router backends
  - [ ] `pkg/bootstrap/` — agent 部署逻辑
  - [ ] `pkg/proto/` — agent wire protocol 定义
  - [ ] `internal/shellquote/` — POSIX 单引号转义
- [ ] CI: lint (golangci-lint) + test (push / PR)
- [ ] 多平台 build matrix: `linux/amd64,arm64`, `darwin/amd64,arm64`
- [ ] LICENSE (MIT)
- [ ] README 骨架 + tagline + 架构图
- [ ] CHANGELOG.md
- [ ] `.editorconfig` + `gofumpt`

### M1 — 核心接口定义

只写接口和类型，零实现。先把契约钉死。

- [ ] `Channel` 接口：`Write / Read / Resize / Close / Caps`
- [ ] `ChannelCaps`：`BinarySafe / SeparateStderr / ScrollbackLimited / MaxWriteChunk / Concurrent`
- [ ] `Session` 接口：`RunFramed(ctx, cmd, stdin) (Result, error)` + `Pipe(ctx, cmd) (io.WriteCloser, io.ReadCloser, ...)` (流式)
- [ ] `Result`：`Stdout / Stderr / ExitCode / Duration`
- [ ] `Backend` 接口：组合 `RemoteFS` + `RemoteExec` + `Probe(ctx) error`
- [ ] `RemoteFS`：`Read / Write / Stat / Lstat / List / Remove / Rename / MkdirAll / OpenRead / OpenWrite`
  - [ ] `OpenRead/OpenWrite` 返回 `io.ReadCloser` / `io.WriteCloser`，避免大文件一次性 buffer
- [ ] `RemoteExec`：`Run(ctx, cmd, stdin) (Result, error)`
- [ ] `FileInfo` 结构（mode、size、mtime、isdir、isSymlink、symlinkTarget）
- [ ] **Op 幂等性标注**：每个 op 在接口注释里标 `ReadOnly / Idempotent / NonIdempotent`，重试与 fallback 决策据此分流
- [ ] 错误类型：`ErrTimeout / ErrCanceled / ErrChannelClosed / ErrAgentMissing / ErrAgentDied / ErrProtocol / ErrTooLarge / ErrCorrupted / ErrTransient`

### M2 — Session 层（sentinel framing）

- [ ] Nonce 生成器（`crypto/rand` 8 字节 hex，对每次调用唯一）
- [ ] 命令包装器：`printf '\n__PR_BEG_%s__\n'; <cmd>; rc=$?; printf '\n__PR_END_%s__:%d\n' "$rc"`
- [ ] `readUntilEnd` 状态机：扫 BEG → 收集 → 扫 END → 解析退出码
- [ ] **Shell 探测**：首次连入时探测 `$0` / `$BASH_VERSION` / `$ZSH_VERSION` / `$KSH_VERSION`，缓存 shell 类型；fish/csh 等不兼容 POSIX 的直接报错
- [ ] **Per-shell prelude**（会话级，一次性）：
  - [ ] 通用：`stty -echo -onlcr -icanon 2>/dev/null; export LC_ALL=C LANG=C; PS1=''; unset PROMPT_COMMAND`
  - [ ] bash/zsh：`set +o history; HISTFILE=/dev/null`
  - [ ] dash/ash：`set +o` 子集（注意 dash 不支持 `set +o history`）
  - [ ] busybox：`stty` 选项有差异，需要 fallback 子集
- [ ] 处理输入回显与 `\r\n` 翻译（即便 prelude 设了 `-onlcr`，prelude 自身的命令仍可能带 CR，状态机需容忍）
- [ ] **ANSI/控制码剥离工具函数**（必做，不再可选）：覆盖 CSI / OSC / 光标控制 / `\b`；状态机匹配 BEG/END 时先经过该层
- [ ] `RunFramed` 互斥锁串行化（同一 Session 上禁止并发）
- [ ] context.Context：超时与取消传播
- [ ] **取消升级链**：Ctrl-C → 等待 `softCancelGrace`（默认 2s） → Ctrl-\\ (SIGQUIT) → 等待 → 标记 Session dead
  - [ ] drain 阶段独立硬超时（默认 5s），避免 `vim` / `less` / 吞 SIGINT 的进程把 Session 卡死
  - [ ] dead Session 的后续调用直接返回 `ErrChannelClosed`
- [ ] 单元测试：用 `mockChannel`（in-memory pipe + 简化 bash 模拟器）覆盖正常 / 超时 / 取消 / 大输出 / 退出码非 0 / sentinel 在用户输出中误碰撞
- [ ] **真 PTY 集成测试矩阵**：`bash` × `zsh` × `dash` × `busybox ash` × `with TTY` × `LC_ALL=C` / `LC_ALL=zh_CN.UTF-8`，验证 prelude 生效前后字节流的状态机兼容性
- [ ] 模糊测试：sentinel parser 对随机字节流不死锁不 panic（亦见横切关注点）

### M3 — ShellBackend

ShellBackend 既是降级方案也是 bootstrap 通道，先于 AgentBackend 实现。

- [ ] `internal/shellquote.Quote(s string) string`（POSIX 单引号转义，`'` → `'\''`）
- [ ] 平台/工具探测缓存：首次 `uname -s` + `command -v` 探测 `base64` / `stat` / `find` / `gzip` / `sha256sum` / `shasum`，决定后续命令风格
- [ ] `Read(path)` — `cat -- 'path' | gzip -1 | base64 -w0` + 本地 `base64 -d | gunzip`（gzip 不可用时退回纯 base64）
- [ ] `Write(path, data, mode)` — **原子写**：写到 `'path'.tmp.<nonce>` → `chmod <mode>` → `mv -- tmp path`，避免半文件可见
- [ ] **写后校验**：`sha256sum 'path'`（或 `shasum -a 256`）与本地比对，失败抛 `ErrCorrupted`
- [ ] `Stat(path)` / `Lstat(path)` — Linux: `stat -c '%n|%s|%Y|%f|%N' --`（`%N` 给符号链接目标）；macOS/BSD fallback: `stat -f '...'`
- [ ] `List(path)` — `find <path> -maxdepth 1 -mindepth 1 -printf '%P\t%s\t%T@\t%y\n'` (Linux) + macOS / busybox 各一个 fallback（`ls -la` 解析作保底）
- [ ] `Remove(path)` — `rm -f --`；递归删用单独的 op `RemoveAll`（标 `NonIdempotent` + 显式确认），避免误删
- [ ] `Rename(old, new)` — `mv -- old new`
- [ ] `MkdirAll(path)` — `mkdir -p --`
- [ ] `Run(cmd, stdin)` — 直接 `Session.RunFramed`，stdin 通过 here-doc 注入；**注意**：here-doc 终止符要避开 nonce sentinel
- [ ] **大文件分块写**：依据 `Caps.MaxWriteChunk` 把 base64 切片 append 到 tempfile，最后 rename；每片写完做长度校验
- [ ] **大文件分块读**：`dd if=path bs=64K skip=N count=1 | base64`，按 chunk 拉取，本地拼接后整体 sha256 校验
- [ ] **流式接口**：`OpenRead/OpenWrite` 分别基于上述分块逻辑，对调用方暴露 `io.ReadCloser/WriteCloser`
- [ ] **busybox 兼容性矩阵**：`base64` (无 `-w0`) / `stat` (BSD 风格) / `find -printf` (不存在) / `sha256sum` (可能没有) — 每条都需要 fallback；维护一个 `compat_test.go` 矩阵跑 alpine / busybox 镜像
- [ ] 大小阈值：超过 `MaxShellFileSize`（默认 4MB）的文件 op 在 ShellBackend 直接返回 `ErrTooLarge`，强制走 AgentBackend

### M4 — Tmux Transport

第一个 Channel 实现，本机就能跑，调试最容易。

- [ ] `TmuxChannel` 配置：socket path、session name、pane id、log dir
- [ ] `tmux` 二进制可用性检查
- [ ] `Write`: `tmux send-keys -l -t <pane> -- <data>`（`-l` 关闭 key binding 翻译）
  - [ ] 单次注入长度限制（保守取 32KB，实测 tmux send-keys 阈值在 64KB 量级；超过分块）
  - [ ] 末尾追加 `Enter` 的策略（让上层显式控制，Channel 不自动加）
  - [ ] **pipe-pane 启动时序**：`Read` 必须在第一次 `Write` 之前就 `pipe-pane` 完成，避免错过早期输出
- [ ] `Read`: 启动 `tmux pipe-pane -t <pane> -o 'cat >> <logfile>'`，本地 tail logfile
  - [ ] **不要**用 `capture-pane` 轮询（scrollback 限制 + ANSI 状态污染）
  - [ ] logfile 自动 rotate（按大小或时间）
  - [ ] Channel.Close 时 `pipe-pane -t <pane>` 关闭
- [ ] `Resize`: `tmux resize-window -t <pane> -x <cols> -y <rows>`
- [ ] `Caps`: `BinarySafe=false, SeparateStderr=false, ScrollbackLimited=false`(因为走 pipe-pane), `MaxWriteChunk=32768`
- [ ] 辅助命令：`ptyrelay tmux-init` 自动起一个 detached session 跑 SSH
- [ ] Shell prelude 由 M2 的 per-shell prelude 机制处理；TmuxChannel 仅负责把 prelude 命令注入到 pane
- [ ] 集成测试：本机 tmux 跑 bash，跑一组 RemoteFS / RemoteExec 操作
- [ ] 集成测试：tmux 内跑 `ssh localhost`，模拟跳板机场景
- [ ] 集成测试：用户在同一 pane 手动输入时，Channel 状态机不被污染（或检测到污染后报错）

### M5 — Agent 协议与远端二进制

- [ ] 协议设计文档 `docs/PROTOCOL.md`
- [ ] 决策：**长寿 REPL 进程**作为 v1 默认（fork 一次，多请求复用，省 fork+解析开销），一次性模式仅作兼容降级
- [ ] **Wire format**：length-prefixed 二进制框架——`4 字节 big-endian 长度 + JSON body`，JSON 中 binary payload 用 base64
  - [ ] 选择 length-prefix 而非行分隔，是为了让 binary safe 通道（WS）也能直接复用，同时避免 base64 解码后 JSON 内换行干扰
  - [ ] 在文本通道（tmux）下，外层仍由 Session 的 sentinel framing 保护
- [ ] 可选 gzip：request/response 头部协商 `"enc":"gzip"`，大 payload 自动启用
- [ ] Request: `{"v":1,"id":"<uuid>","op":"read","args":{...}}`（`id` 用于 REPL 模式下匹配响应）
- [ ] Response: `{"v":1,"id":"<uuid>","ok":true,"data":...}` 或 `{"v":1,"id":"<uuid>","ok":false,"err":"...","errKind":"..."}`
- [ ] Op 集合（与 RemoteFS/RemoteExec 一一对应）：`ping / read / write / stat / lstat / list / remove / rename / mkdir / run / open_read / open_write`
- [ ] 协议版本协商：`ping` 返回 `{"v":1,"agent":"<sha256>","version":"...","caps":[...]}`
- [ ] **进程模型**：
  - [ ] `ptyrelay-agent --repl`：启动后独占 Session，循环读 length-prefixed 消息直到 EOF / `bye` op
  - [ ] `ptyrelay-agent`（无参数）：单 op 后退出，仅在远端不允许长寿进程或 REPL 协议失败时 fallback
  - [ ] `ptyrelay-agent --version`：仅打印版本，用于 bootstrap 后 probe
- [ ] `cmd/ptyrelay-agent/main.go`：dispatch + 严格的输入校验（拒绝未知 op、长度上限）
- [ ] 静态编译: `CGO_ENABLED=0 GOOS=... GOARCH=...`
- [ ] 编译产物大小优化: `-ldflags="-s -w"` + 可选 `upx --best`（评估解压时间）
- [ ] 多平台二进制 build 脚本（`scripts/build-agents.sh`）
- [ ] 二进制 embed 到本地 CLI: `//go:embed agents/*.gz`（embed 时已 gzip，省启动时压缩）
- [ ] `AgentBackend` 实现：
  - [ ] **REPL 模式**：通过 `Session.Pipe` 拿到独占的 stdin/stdout，启动 agent，多 op 复用同一进程
  - [ ] **一次性模式**：`Session.RunFramed("./ptyrelay-agent <<'__PR_AGENT_EOF__'\n{...}\n__PR_AGENT_EOF__")`（here-doc 终止符与 sentinel 不同名）
  - [ ] 协议失败 / 进程崩溃 → 抛 `ErrAgentDied`，由 RouterBackend 决策
- [ ] AgentBackend 路径可配置（默认 `~/.local/bin/ptyrelay-agent`）

### M6 — Bootstrap 与 Router

- [ ] 远端平台探测：`ShellBackend.Run("uname -sm")` → 选匹配 embed 的二进制
- [ ] 部署目录决策（按存在性优先级）：`$XDG_DATA_HOME/ptyrelay/` → `~/.local/bin/` → `~/.ptyrelay/`
- [ ] **上传策略分级**（按速度递减、可用性递增，自动协商）：
  1. **远端有外网 + curl/wget**：`curl -fsSL <release-url>/ptyrelay-agent-<os>-<arch>.gz | gunzip > path && chmod +x path`，本地校验 sha256
  2. **远端有 base64 + gzip**：本地 `gzip -9` 二进制 → base64 → 远端 `base64 -d | gunzip > tmp && mv tmp path`（典型 5MB → 1.5–2MB）
  3. **极简系统（base64 only）**：`ShellBackend.Write` 分块 base64 写 tempfile → rename（保底路径）
  4. **极极简（无 base64）**：`od -An -tx1` 编码 + 远端 `xxd -r -p` 解码（再保底，能不能用看用户运气）
- [ ] 大块上传：`MaxWriteChunk` 上调到 32–64KB（实测 `tmux send-keys` 阈值 ~64KB；超过会被 tmux 截断）
- [ ] 上传流程统一收尾：`mkdir -p` → 写到 tempfile → `chmod +x` → `mv` → 写 `.sha256` → 远端 `sha256sum` 校验
- [ ] 部署后 probe：`AgentBackend.Ping()` 检查 version + caps + sha256 与本地 embed 一致
- [ ] **`RouterBackend`**（重命名自 `FallbackBackend`，反映其路由+自愈职责，而不仅是降级）：
  - [ ] 构造时 probe agent
  - [ ] agent 可用 → op 默认走 Agent
  - [ ] agent 不可用 → op 走 Shell（仅当 op 在 Shell 上可表达且 size 未超 `MaxShellFileSize`）
  - [ ] **运行中 agent 报 `ErrAgentDied`，按 op 幂等性分流**：
    - `ReadOnly` (read/stat/lstat/list/ping) → 自动 fallback 到 Shell
    - `Idempotent` (mkdir/rename) → 自动 fallback 到 Shell
    - `NonIdempotent` (write/remove/run) → 上抛 `ErrAgentDied` 由调用方决策（避免重复执行）
  - [ ] 标记 Session 需要 re-bootstrap，下次进入前自愈
- [ ] 自愈：下次 ReadOnly op 前自动 re-bootstrap，避免在关键路径上重新上传二进制
- [ ] 显式 CLI: `ptyrelay re-bootstrap` 强制重装
- [ ] 显式 CLI: `ptyrelay agent-info` 打印远端 agent 版本/路径/校验和

### M7 — WebSocket Transport (code-local)

- [ ] 对接 code-local: 阅读 `terminal.RunSession` 与 `remoteterminal channel` 接口
- [ ] `WebSocketChannel` 实现
  - [ ] `Write` → 写 data 帧到 channel
  - [ ] `Read` → 从 onData 事件拼接（带 buffered reader）
  - [ ] `Resize` → 发 resize 控制帧
  - [ ] `Close` → 关闭 channel
- [ ] `Caps`: `BinarySafe=true, SeparateStderr=取决于是否分配 PTY, ScrollbackLimited=false, MaxWriteChunk=0`
- [ ] 探索：是否能配置 code-server 不分配 PTY（这样 stderr 可独立通道，性能更好）
- [ ] 重连逻辑：短暂断网时缓存请求 / 重新建链
- [ ] 无修改地复用 ShellBackend / AgentBackend / RouterBackend
- [ ] 集成测试：本地起 code-server + ptyrelay 跑同一组 op，结果与 tmux 路径一致（确认抽象闭合）

### M8 — 健壮性

- [ ] `context.Context` 全链路传递（无 background context 漏网）
- [ ] 默认操作超时（30s 可配）
- [ ] **取消升级链**（与 M2 统一实现）：Ctrl-C → 软超时（默认 2s）→ Ctrl-\\ → 硬超时（默认 5s）→ Session dead；drain 阶段独立硬超时
- [ ] Channel 异常关闭检测（write 返回 EPIPE / read EOF）→ 标记 Channel dead，禁止后续调用
- [ ] panic recovery（agent 协议解析、状态机）
- [ ] 结构化日志 (`log/slog`)：Channel write / read 字节数、op 名、耗时、错误码
- [ ] 敏感数据脱敏：base64 payload、文件内容默认不打日志（`--debug` 模式下也只打前 256 字节）
- [ ] 错误分类：transient (重试) / permanent (上抛) / fatal (Channel dead)
- [ ] **Op 幂等性矩阵**（与 M1 标注 + RouterBackend 决策对齐）：
  | Op | Class | 自动重试 | agent 死时自动 fallback |
  |---|---|---|---|
  | ping/read/stat/lstat/list | ReadOnly | ✓ | ✓ |
  | mkdir/rename | Idempotent | ✓ | ✓ |
  | write | Idempotent\* | 仅在原子写未提交前 | ✓（带写后校验） |
  | remove | NonIdempotent | ✗ | ✗ |
  | run | NonIdempotent | ✗ | ✗ |
  - \* `write` 因为走 tempfile + rename，重写整个 op 是安全的，但分块 append 中途失败需要从 tempfile 起始重来
- [ ] 重试策略：agent ping 失败重试 3 次（指数退避 200ms→1s→4s），仍失败 fallback 到 shell；非幂等 op 上抛 `ErrTransient`

### M9 — 性能优化

> 注：gzip + 流式 + REPL 已分别落到 M3 / M1 / M5。M9 聚焦运行时优化与基准。

- [ ] **Channel "独占 lease" 语义**：REPL agent 占用期间禁止其它 RunFramed；lease 释放后回到普通 framed 模式
- [ ] REPL ↔ 一次性模式的运行时切换：远端不允许长寿进程时降级
- [ ] Agent 协议大 payload 的 gzip 阈值调优（默认 `>4KB` 启用）
- [ ] Benchmark 套件：文件 IO 吞吐、RPC 延迟、并发上限（tmux vs WS）、REPL vs 一次性进程对比
- [ ] tmux pipe-pane 的 logfile rotate（按大小 100MB 或按 op 数）
- [ ] 连接复用：同一个 (host, session) 缓存 RouterBackend，避免每次 CLI 调用重新 probe + bootstrap

### M10 — 集成到 Claude Code / 调用方

- [ ] Go library API 整理（公开 vs internal）
- [ ] CLI 子命令：
  - [ ] `ptyrelay exec --tmux <session> -- <cmd>`
  - [ ] `ptyrelay get --tmux <session> <path>`
  - [ ] `ptyrelay put --tmux <session> <local> <remote>`
  - [ ] `ptyrelay --ws <addr> ...` 同上
- [ ] MCP server 包装（让 Claude Code 直接发现 ptyrelay 作为 tool provider）
- [ ] code-local 集成 hook：暴露 `ptyrelay.AttachToSession(s)` API
- [ ] 端到端 demo：Claude Code → ptyrelay → 远端容器，跑一个真实调试场景

### M11 — 文档与发布

- [ ] README.md：动机故事 + 架构图 + quickstart
- [ ] docs/ARCHITECTURE.md：分层设计、扩展点、为什么这么切
- [ ] docs/PROTOCOL.md：agent wire protocol 完整规范
- [ ] docs/TRANSPORTS.md：tmux / WS 各自的注意事项
- [ ] docs/SECURITY.md：威胁模型、shell injection 防御、agent 完整性
- [ ] CONTRIBUTING.md
- [ ] Demo: asciinema 录屏 + GIF
- [ ] GitHub Release：多平台 binary + checksums + signed
- [ ] go.dev 包发布
- [ ] 软文（可选）：HN / Reddit / dev.to

---

## 横切关注点

### 测试

- [ ] 单元测试覆盖率目标 ≥ 70%
- [ ] 集成测试：本机 tmux + bash
- [ ] 集成测试：本机 tmux + `ssh localhost`（模拟跳板机）
- [ ] 集成测试：本机起 code-server + WS Channel
- [ ] e2e：Docker compose 起 jumphost + target，跑完整工作流
- [ ] 模糊测试：sentinel parser 喂随机字节流不能死锁不能 panic
- [ ] busybox / alpine / ubuntu / centos 7 / macOS 远端兼容矩阵

### 安全

- [ ] 所有用户输入路径走 `shellquote.Quote`
- [ ] 静态扫描禁止 `fmt.Sprintf("...%s...", path)` 模式（自定义 lint rule）
- [ ] agent 二进制嵌入时记录 sha256，部署后远端验证
- [ ] agent 协议拒绝未知 op（避免后续版本注入老 agent）
- [ ] 不在日志里记录 base64 payload
- [ ] 拒绝远端路径含 `\x00` 或换行符

### 可观测

- [ ] `--debug` 详细模式：打印每条 send-keys 内容、每个 op 的 wire bytes
- [ ] 可选 `--trace-file` 把所有 channel 字节流落盘供事后分析
- [ ] op 计数 / 失败率 / 平均延迟（可暴露 Prometheus，optional）

---

## 未来路线（Nice-to-have）

- [ ] `KubectlExecChannel` — 通过 `kubectl exec -it` 进 Pod
- [ ] `DockerExecChannel` — 通过 docker exec 进容器
- [ ] `SerialChannel` — 串口 / 物理设备控制台
- [ ] Windows 远端支持（PowerShell / WSL bash）
- [ ] agent 协议加密层（chacha20-poly1305，预共享密钥）
- [ ] 多 Channel 并发（一个远端 host 上多个 agent 实例并行）
- [ ] TUI 调试器：实时观察 agent 通信
- [ ] WASM agent（用于无法上传二进制但有 Node/Python 的环境）

---

## 风险登记表

| 风险 | 影响 | 缓解 |
|---|---|---|
| code-local wire protocol 漂移 | WSChannel 周期性失效 | 钉版本 + CI 兼容性测试 |
| `tmux send-keys` 长字符串截断 | 大文件写入静默丢字节 | MaxWriteChunk + 写后 sha256 校验 |
| 远端缺 `base64` 命令（极简系统） | ShellBackend bootstrap 失败 | 上传策略分级（curl / base64+gzip / od+xxd），仍失败要求用户预装 |
| 远端 stat / ls 格式跨平台差异 | List/Stat 解析错误 | 工具探测缓存 + 多解析器 + busybox 兼容矩阵 |
| Agent 进程崩溃但 prompt 未恢复 | Session 卡死 | 取消升级链（Ctrl-C → Ctrl-\\）+ drain 硬超时 + dead-channel detection |
| 非幂等 op 在 agent 死亡时盲目 fallback | 命令重复执行 / 数据损坏 | RouterBackend 按幂等性分流，NonIdempotent 上抛 `ErrAgentDied` 由调用方决策 |
| REPL agent 协议解析卡住 | 整个 Session 失活 | length-prefixed + per-request id + 进程级硬超时强杀 |
| Sentinel 在用户输出中误碰撞 | 解析错位 | nonce + sentinel parser 模糊测试 |
| 用户在 tmux session 里手动操作干扰 | 数据竞争 | 推荐独立 session/window，README 警示 |
| 二进制 embed 让本地 CLI 体积膨胀 | 分发不便 | embed `.gz` + 按需下载模式（首次运行从 release 拉对应平台二进制） |
| `vim` / `less` 等 TUI 程序吞 SIGINT | 取消失效，Session 卡死 | Ctrl-C 软超时后升级到 Ctrl-\\，再超时则 Channel dead |

---

## 命名约定

- 包名小写、单词、无下划线：`channel`、`session`、`backend`
- 接口名：动词 + er，或名词直接命名（`Channel` / `Backend`）
- 协议常量加前缀：`__PR_BEG_` / `__PR_END_`（PR = ptyrelay）
- 错误变量：`Err<Reason>`（`ErrTimeout`、`ErrAgentMissing`）

---

## 发布节奏

抽象第一，性能第二，集成第三。三个 minor 版本各自有独立可验证的价值。

### v0.1.0 — Shell-only MVP（**先把 Channel/Session/Backend 抽象跑通**）

不依赖 agent，先让 Shell 路径可用，验证分层设计。这一版"慢但通"，让外部用户能尽早试错抽象边界。

- [ ] M0 全部
- [ ] M1 全部（含 Lstat / OpenRead / OpenWrite / 幂等性标注）
- [ ] M2 全部（含 per-shell prelude / PTY 测试矩阵 / 取消升级链 / sentinel 模糊测试）
- [ ] M3 全部（原子写 / 写后 sha256 / 流式接口 / busybox 兼容矩阵）
- [ ] M4 全部（send-keys / pipe-pane / Caps / `tmux-init` 辅助命令）
- [ ] M11：README + quickstart + ARCHITECTURE.md 骨架

### v0.2.0 — Agent 路径，性能可用

加上 RouterBackend 和长寿 agent，把延迟从"百毫秒级"降到"毫秒级"。

- [ ] M5 全部（REPL 默认 + length-prefixed + 全 op）
- [ ] M6 全部（分级上传 + RouterBackend + 自愈）
- [ ] M8 全部（取消升级链、idempotent 重试、错误分类）
- [ ] M11：PROTOCOL.md + SECURITY.md

### v0.3.0 — 第二个 transport + 集成

证明抽象闭合：换一个 Channel，不改 Backend。

- [ ] M7 全部（WS transport）
- [ ] M9（按需，至少跑出 benchmark）
- [ ] M10 全部（MCP 包装、CLI 子命令、code-local 集成）
- [ ] M11：TRANSPORTS.md + demo 录屏