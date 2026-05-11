// Command ptyrelay is the ptyrelay CLI.
//
// It picks a transport (--tmux or --ws), wraps it in Session +
// RouterBackend, and exposes the resulting RemoteFS/RemoteExec surface
// through a handful of subcommands.
//
//	ptyrelay <subcommand> [transport flags] [args]
//	         --tmux <pane> | --ws <url>
//
// Subcommands:
//
//	exec <cmd>               run a remote command
//	get <path>               read remote file to stdout
//	put <local> <remote>     write local file to remote (atomic)
//	stat <path>              print remote file metadata
//	ls <path>                list a remote directory
//	bootstrap                install the agent on the remote
//	agent-info               print agent path / version / sha256
//
// Run `ptyrelay help` for per-subcommand flags.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}
	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "exec":
		os.Exit(cmdExec(args))
	case "get":
		os.Exit(cmdGet(args))
	case "put":
		os.Exit(cmdPut(args))
	case "stat":
		os.Exit(cmdStat(args))
	case "ls":
		os.Exit(cmdList(args))
	case "bootstrap":
		os.Exit(cmdBootstrap(args))
	case "agent-info":
		os.Exit(cmdAgentInfo(args))
	case "help", "-h", "--help":
		usage(os.Stdout)
		os.Exit(0)
	case "version", "--version":
		fmt.Println("ptyrelay v0.3.0")
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "ptyrelay: unknown subcommand %q\n\n", sub)
		usage(os.Stderr)
		os.Exit(2)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `Usage: ptyrelay <subcommand> [transport flags] [args]

Subcommands:
  exec <cmd>             Run a remote shell command
  get  <path>            Read a remote file to stdout
  put  <local> <remote>  Atomically write a local file to the remote
  stat <path>            Print remote file metadata
  ls   <path>            List a remote directory
  bootstrap              Install the agent binary on the remote
  agent-info             Print remote agent path / version

Transport flags (one of --tmux, --ws or --exec is required):
  --tmux <pane>          Use a tmux pane (e.g. "my-sess:0.0")
  --tmux-socket <path>   Optional tmux socket path
  --ws <url>             Use a WebSocket bridge (ws:// or wss://)
  --ws-header k=v        Extra header for the WS upgrade (repeatable)
  --exec "<argv>"        Launch a local command and use its stdio
                         (e.g. "docker exec -i container bash",
                          "kubectl exec -i -n ns pod -- bash",
                          "ssh -T user@host bash")

Backend flags:
  --no-agent             Force ShellBackend only (skip the agent path)
  --agent <path>         Remote agent path (default ~/.local/bin/ptyrelay-agent)
  --install              If the agent is missing, run bootstrap first
  --provider-dir <dir>   For --install: directory of agent binaries laid
                         out as <dir>/<os>-<arch>
  --shell <name>         Remote shell: bash|zsh|dash|sh (default bash)
  --timeout <dur>        Overall context timeout (default 60s)

Examples:
  ptyrelay exec --tmux work:0.0 -- uname -a
  ptyrelay get  --ws ws://localhost:8765/term /etc/hostname
  ptyrelay put  --tmux work:0.0 --install --provider-dir ./dist  ./local /remote
`)
}
