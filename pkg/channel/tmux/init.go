package tmux

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// InitOptions configures [InitSession].
type InitOptions struct {
	// Socket / SocketIsPath select the tmux server (see Options).
	Socket       string
	SocketIsPath bool

	// SessionName names the new tmux session. Required.
	SessionName string

	// Command is the shell or program to start in the initial pane.
	// Empty means tmux's default shell.
	Command string

	// Width / Height set the initial geometry. 0 uses tmux defaults.
	Width, Height int
}

// InitSession creates a new detached tmux session and returns a
// fully-populated [Options] pointing at the session's first pane —
// suitable for passing to [New].
//
// The session is left running; callers are responsible for tearing it
// down (typically via `tmux kill-session -t <name>` or by killing the
// whole server). The resolved pane spec is returned via Options.Pane.
func InitSession(ctx context.Context, opts InitOptions) (Options, error) {
	if opts.SessionName == "" {
		return Options{}, errors.New("tmux: SessionName is required")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return Options{}, fmt.Errorf("tmux: binary not found: %w", err)
	}

	prefix := socketArgs(opts.Socket, opts.SocketIsPath)

	args := []string{"new-session", "-d", "-s", opts.SessionName}
	if opts.Width > 0 {
		args = append(args, "-x", fmt.Sprint(opts.Width))
	}
	if opts.Height > 0 {
		args = append(args, "-y", fmt.Sprint(opts.Height))
	}
	if opts.Command != "" {
		args = append(args, opts.Command)
	}
	if err := runTmux(ctx, prefix, args...); err != nil {
		return Options{}, err
	}

	// Resolve the pane id so the caller has a stable target even if
	// the session is later renamed.
	full := append(append([]string{}, prefix...), "list-panes", "-t", opts.SessionName, "-F", "#{pane_id}")
	cmd := exec.CommandContext(ctx, "tmux", full...)
	out, err := cmd.Output()
	if err != nil {
		return Options{}, fmt.Errorf("tmux: list-panes: %w", err)
	}
	paneID := strings.TrimSpace(string(out))
	if paneID == "" {
		return Options{}, errors.New("tmux: list-panes returned no pane")
	}
	// list-panes may emit multiple lines if the session has multiple
	// panes; we created one window with a single pane, so take the
	// first.
	if i := strings.IndexByte(paneID, '\n'); i >= 0 {
		paneID = paneID[:i]
	}

	return Options{
		Pane:         paneID,
		Socket:       opts.Socket,
		SocketIsPath: opts.SocketIsPath,
	}, nil
}

// KillSession tears down a session previously created by InitSession.
// Best-effort — errors are returned but most callers can ignore them
// during cleanup.
func KillSession(ctx context.Context, opts InitOptions) error {
	prefix := socketArgs(opts.Socket, opts.SocketIsPath)
	return runTmux(ctx, prefix, "kill-session", "-t", opts.SessionName)
}
