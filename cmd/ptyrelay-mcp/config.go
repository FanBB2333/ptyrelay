package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/router"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/channel"
	"github.com/FanBB2333/ptyrelay/pkg/channel/tmux"
	"github.com/FanBB2333/ptyrelay/pkg/channel/websocket"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// config bundles every parameter that controls how the server talks to
// the remote. Populated from environment variables once at startup.
type config struct {
	Transport string // "tmux" | "ws"
	TmuxPane  string
	TmuxSock  string
	WSURL     string
	Shell     session.ShellKind
	AgentPath string
	NoAgent   bool
	Timeout   time.Duration
}

func loadConfig() (*config, error) {
	c := &config{
		Transport: os.Getenv("PTYRELAY_TRANSPORT"),
		TmuxPane:  os.Getenv("PTYRELAY_TMUX_PANE"),
		TmuxSock:  os.Getenv("PTYRELAY_TMUX_SOCK"),
		WSURL:     os.Getenv("PTYRELAY_WS_URL"),
		AgentPath: os.Getenv("PTYRELAY_AGENT"),
		NoAgent:   truthy(os.Getenv("PTYRELAY_NO_AGENT")),
	}

	switch c.Transport {
	case "tmux":
		if c.TmuxPane == "" {
			return nil, errors.New("PTYRELAY_TRANSPORT=tmux requires PTYRELAY_TMUX_PANE")
		}
	case "ws":
		if c.WSURL == "" {
			return nil, errors.New("PTYRELAY_TRANSPORT=ws requires PTYRELAY_WS_URL")
		}
	case "":
		return nil, errors.New("PTYRELAY_TRANSPORT not set (want tmux|ws)")
	default:
		return nil, fmt.Errorf("PTYRELAY_TRANSPORT=%q unsupported (want tmux|ws)", c.Transport)
	}

	switch strings.ToLower(os.Getenv("PTYRELAY_SHELL")) {
	case "", "bash":
		c.Shell = session.ShellBash
	case "zsh":
		c.Shell = session.ShellZsh
	case "dash":
		c.Shell = session.ShellDash
	case "sh":
		c.Shell = session.ShellSh
	default:
		return nil, fmt.Errorf("PTYRELAY_SHELL=%q unsupported", os.Getenv("PTYRELAY_SHELL"))
	}

	c.Timeout = 60 * time.Second
	if v := os.Getenv("PTYRELAY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("PTYRELAY_TIMEOUT=%q: %v", v, err)
		}
		c.Timeout = d
	}
	return c, nil
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// dialFromConfig opens the configured transport, constructs Backend, and
// returns it along with a teardown closure.
func dialFromConfig(ctx context.Context, c *config) (backend.Backend, func(), error) {
	dctx, dcancel := context.WithTimeout(ctx, c.Timeout)
	defer dcancel()

	var ch channel.Channel
	var err error
	switch c.Transport {
	case "tmux":
		ch, err = tmux.New(dctx, tmux.Options{
			Pane:         c.TmuxPane,
			Socket:       c.TmuxSock,
			SocketIsPath: c.TmuxSock != "",
		})
	case "ws":
		ch, err = websocket.Dial(dctx, websocket.Options{URL: c.WSURL})
	}
	if err != nil {
		return nil, nil, fmt.Errorf("transport: %w", err)
	}

	sess := session.New(ch, c.Shell)
	sb := shell.New(sess)
	teardown := func() {
		_ = sess.Close()
		_ = ch.Close()
	}

	if c.NoAgent {
		return sb, teardown, nil
	}

	agentPath := c.AgentPath
	if agentPath == "" {
		res, rerr := sb.Run(dctx, "printf '%s' \"$HOME\"", nil)
		if rerr != nil {
			teardown()
			return nil, nil, fmt.Errorf("resolve $HOME: %w", rerr)
		}
		home := strings.TrimSpace(string(res.Stdout))
		if home == "" {
			teardown()
			return nil, nil, errors.New("remote $HOME empty; set PTYRELAY_AGENT")
		}
		agentPath = home + "/.local/bin/ptyrelay-agent"
	}
	ab := agent.New(sess, agentPath)
	rb := router.New(ab, sb)
	_ = rb.Probe(dctx)
	return rb, teardown, nil
}
