package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/router"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
	"github.com/FanBB2333/ptyrelay/pkg/channel"
	"github.com/FanBB2333/ptyrelay/pkg/channel/subprocess"
	"github.com/FanBB2333/ptyrelay/pkg/channel/tmux"
	"github.com/FanBB2333/ptyrelay/pkg/channel/websocket"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// commonFlags collects the transport + backend flags shared by every
// subcommand.
type commonFlags struct {
	tmuxPane   string
	tmuxSocket string

	wsURL    string
	wsHeader stringList

	// execCmd is the full argv for a subprocess-backed transport, e.g.
	// `docker exec -i container bash`. Parsed via whitespace fields —
	// wrap complex commands in `bash -c '…'`.
	execCmd string

	shellName string
	timeout   time.Duration

	noAgent     bool
	agentPath   string
	doInstall   bool
	providerDir string

	logLevel string // "", "debug", "info", "warn", "error"
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.tmuxPane, "tmux", "", "tmux pane target (e.g. session:0.0)")
	fs.StringVar(&c.tmuxSocket, "tmux-socket", "", "tmux socket path (-S form)")
	fs.StringVar(&c.wsURL, "ws", "", "WebSocket URL (ws:// or wss://)")
	fs.Var(&c.wsHeader, "ws-header", "WS upgrade header k=v (repeatable)")
	fs.StringVar(&c.execCmd, "exec", "", `subprocess transport: local argv to launch, e.g. "docker exec -i container bash"`)
	fs.StringVar(&c.shellName, "shell", "bash", "remote shell: bash|zsh|dash|sh")
	fs.DurationVar(&c.timeout, "timeout", 60*time.Second, "overall context timeout")

	fs.BoolVar(&c.noAgent, "no-agent", false, "force ShellBackend only")
	fs.StringVar(&c.agentPath, "agent", "", "remote agent path (default ~/.local/bin/ptyrelay-agent)")
	fs.BoolVar(&c.doInstall, "install", false, "auto-bootstrap agent if missing")
	fs.StringVar(&c.providerDir, "provider-dir", "", "agent binaries directory for --install")
	fs.StringVar(&c.logLevel, "log-level", "", "structured log level: debug|info|warn|error (default: silent)")
}

// stringList is a repeatable flag value.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// connection is what subcommands receive: a live Backend with a known
// shutdown hook.
type connection struct {
	Ctx     context.Context
	Backend backend.Backend // router (default) or shell (--no-agent)
	Shell   *shell.Backend  // always present, useful for bootstrap-style ops
	Agent   *agent.Backend  // nil when --no-agent

	close func()
}

func (c *connection) Close() {
	if c.close != nil {
		c.close()
	}
}

// dial builds Channel → Session → Backend per the parsed flags.
func dial(c *commonFlags) (*connection, error) {
	picked := 0
	for _, v := range []string{c.tmuxPane, c.wsURL, c.execCmd} {
		if v != "" {
			picked++
		}
	}
	if picked != 1 {
		return nil, errors.New("exactly one of --tmux, --ws or --exec is required")
	}
	shellKind, err := parseShell(c.shellName)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)

	var ch channel.Channel
	switch {
	case c.tmuxPane != "":
		ch, err = tmux.New(ctx, tmux.Options{
			Pane:         c.tmuxPane,
			Socket:       c.tmuxSocket,
			SocketIsPath: c.tmuxSocket != "",
		})
	case c.wsURL != "":
		hdr := http.Header{}
		for _, kv := range c.wsHeader {
			i := strings.IndexByte(kv, '=')
			if i < 0 {
				cancel()
				return nil, fmt.Errorf("--ws-header %q: expected key=value", kv)
			}
			hdr.Add(kv[:i], kv[i+1:])
		}
		ch, err = websocket.Dial(ctx, websocket.Options{URL: c.wsURL, Header: hdr})
	case c.execCmd != "":
		argv := strings.Fields(c.execCmd)
		if len(argv) == 0 {
			cancel()
			return nil, errors.New("--exec: empty command")
		}
		ch, err = subprocess.Start(ctx, subprocess.Options{Command: argv})
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("transport: %w", err)
	}

	sess := session.New(ch, shellKind)
	log, err := buildLogger(c.logLevel)
	if err != nil {
		_ = ch.Close()
		cancel()
		return nil, err
	}
	sb := shell.New(sess, shell.WithLogger(log))

	teardown := func() {
		_ = sess.Close()
		_ = ch.Close()
		cancel()
	}

	if c.noAgent {
		return &connection{
			Ctx:     ctx,
			Backend: sb,
			Shell:   sb,
			close:   teardown,
		}, nil
	}

	if c.doInstall {
		var provider bootstrap.Provider
		switch {
		case c.providerDir != "":
			provider = &bootstrap.FileProvider{Dir: c.providerDir}
		default:
			provider = embeddedProvider() // nil unless -tags embedagents
		}
		if provider == nil {
			teardown()
			return nil, errors.New("--install requires --provider-dir (or a binary built with -tags embedagents)")
		}
		path, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
			Provider:    provider,
			InstallPath: c.agentPath,
		})
		if err != nil {
			teardown()
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
		c.agentPath = path
	}

	if c.agentPath == "" {
		// Default agent path: resolve $HOME on the remote.
		res, err := sb.Run(ctx, "printf '%s' \"$HOME\"", nil)
		if err != nil {
			teardown()
			return nil, fmt.Errorf("resolve remote $HOME: %w", err)
		}
		home := strings.TrimSpace(string(res.Stdout))
		if home == "" {
			teardown()
			return nil, errors.New("remote $HOME is empty; pass --agent explicitly")
		}
		c.agentPath = home + "/.local/bin/ptyrelay-agent"
	}

	ab := agent.New(sess, c.agentPath, agent.WithLogger(log))
	rb := router.New(ab, sb, router.WithLogger(log))
	_ = rb.Probe(ctx) // probe is best-effort; fallback works even if agent's down

	return &connection{
		Ctx:     ctx,
		Backend: rb,
		Shell:   sb,
		Agent:   ab,
		close:   teardown,
	}, nil
}

func parseShell(name string) (session.ShellKind, error) {
	switch strings.ToLower(name) {
	case "", "bash":
		return session.ShellBash, nil
	case "zsh":
		return session.ShellZsh, nil
	case "dash":
		return session.ShellDash, nil
	case "sh":
		return session.ShellSh, nil
	default:
		return 0, fmt.Errorf("unsupported --shell %q (want bash|zsh|dash|sh)", name)
	}
}

// fail prints an error and returns a non-zero exit code for main.go.
func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "ptyrelay: "+format+"\n", args...)
	return 1
}

// buildLogger maps the --log-level flag to a configured *slog.Logger.
// Empty (default) returns nil — backends will silently no-op.
func buildLogger(level string) (*slog.Logger, error) {
	if level == "" {
		return nil, nil
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("--log-level: unknown value %q (want debug|info|warn|error)", level)
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), nil
}
