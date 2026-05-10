// Package tmux provides a [channel.Channel] backed by a tmux pane.
//
// Bytes flow:
//
//   - Write → `tmux send-keys -l -t <pane> -- <bytes>`. The `-l` flag
//     puts send-keys in literal mode: every byte is delivered to the
//     pane's PTY input as-is rather than being looked up as a tmux key
//     name (so `\x03` sends Ctrl-C, not the three-character string
//     `C-c`). Long payloads are split because send-keys has an internal
//     length limit (~64 KiB on most builds).
//
//   - Read pulls bytes from a logfile that tmux's pipe-pane writes to.
//     We deliberately do not use `capture-pane` — capture-pane reads the
//     pane's scrollback buffer, which is bounded and post-processed
//     (ANSI state collapsed). pipe-pane streams every byte the pane
//     produces, exactly as it produces them.
//
// TmuxChannel reports BinarySafe=false: send-keys does pass arbitrary
// bytes through, but the receiving shell + tmux's terminal emulation may
// reinterpret NUL bytes and certain control sequences. The Session layer
// handles cooked-mode echo and CRLF translation already.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
)

const (
	defaultChunkSize    = 32 * 1024
	defaultPollInterval = 20 * time.Millisecond
)

// Options configures [New].
type Options struct {
	// Pane identifies the tmux pane to talk to. Accepted forms include
	// `%<id>` (a pane id), `<session>:<window>.<pane>`, or any other
	// target spec tmux understands.
	Pane string

	// Socket is the optional `-L <name>` or `-S <path>` socket. Empty
	// means tmux's default socket.
	Socket string

	// SocketIsPath, when true, passes Socket via `-S`; otherwise via
	// `-L`. The two flags are mutually exclusive in tmux's CLI.
	SocketIsPath bool

	// LogFile is the path tmux's pipe-pane writes to. Empty means a
	// temp file is created and removed on Close.
	LogFile string

	// ChunkSize bounds send-keys payload size. Default 32 KiB.
	ChunkSize int

	// PollInterval is how often Read polls the logfile for new bytes
	// when its current view is empty. Default 20ms — a tradeoff
	// between latency and CPU when idle.
	PollInterval time.Duration
}

// Channel is the [channel.Channel] implementation over a tmux pane.
type Channel struct {
	pane         string
	logFile      *os.File
	logPath      string
	logIsTemp    bool
	chunkSize    int
	pollInterval time.Duration

	tmuxArgs []string // pre-built socket prefix args

	closeOnce sync.Once
	closed    atomic.Bool
	doneCh    chan struct{}
}

// New attaches a Channel to an existing tmux pane. The pane must already
// exist (use the `tmux-init` helper or any tmux command to create one).
func New(ctx context.Context, opts Options) (*Channel, error) {
	if opts.Pane == "" {
		return nil, errors.New("tmux: Pane is required")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return nil, fmt.Errorf("tmux: binary not found in PATH: %w", err)
	}

	chunk := opts.ChunkSize
	if chunk <= 0 {
		chunk = defaultChunkSize
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}

	tmuxArgs := socketArgs(opts.Socket, opts.SocketIsPath)

	// Confirm the pane exists before we start piping into it; a typo
	// here is the most common failure mode.
	if err := runTmux(ctx, tmuxArgs, "display-message", "-p", "-t", opts.Pane, ""); err != nil {
		return nil, fmt.Errorf("tmux: pane %q not found: %w", opts.Pane, err)
	}

	logPath, isTemp, err := resolveLogPath(opts.LogFile)
	if err != nil {
		return nil, err
	}

	// Truncate any pre-existing log so Read starts on a known
	// boundary; tmux's pipe-pane appends.
	if err := os.Truncate(logPath, 0); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("tmux: truncate log: %w", err)
	}

	// pipe-pane "-o" toggles. Without "-o" tmux *adds* a pipe; "-o"
	// disables any earlier pipe before adding the new one — keeps us
	// from accidentally double-recording into stale logs.
	pipeCmd := fmt.Sprintf("cat >> %s", shellSingleQuote(logPath))
	if err := runTmux(ctx, tmuxArgs, "pipe-pane", "-o", "-t", opts.Pane, pipeCmd); err != nil {
		return nil, fmt.Errorf("tmux: pipe-pane: %w", err)
	}

	logFile, err := os.Open(logPath)
	if err != nil {
		_ = runTmux(context.Background(), tmuxArgs, "pipe-pane", "-t", opts.Pane)
		if isTemp {
			_ = os.Remove(logPath)
		}
		return nil, fmt.Errorf("tmux: open log: %w", err)
	}

	return &Channel{
		pane:         opts.Pane,
		logFile:      logFile,
		logPath:      logPath,
		logIsTemp:    isTemp,
		chunkSize:    chunk,
		pollInterval: poll,
		tmuxArgs:     tmuxArgs,
		doneCh:       make(chan struct{}),
	}, nil
}

// Read implements [channel.Channel].
//
// Read polls the logfile when no new bytes are available; the poll
// interval is configurable (default 20ms). It blocks until either bytes
// arrive or the channel is closed.
func (c *Channel) Read(p []byte) (int, error) {
	for {
		if c.closed.Load() {
			return 0, channel.ErrChannelClosed
		}
		n, err := c.logFile.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		// EOF means "no more bytes right now"; wait and retry.
		select {
		case <-time.After(c.pollInterval):
		case <-c.doneCh:
			return 0, channel.ErrChannelClosed
		}
	}
}

// Write implements [channel.Channel].
func (c *Channel) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, channel.ErrChannelClosed
	}
	written := 0
	for off := 0; off < len(p); {
		end := off + c.chunkSize
		if end > len(p) {
			end = len(p)
		}
		// send-keys -l: every arg is treated as literal text.
		if err := runTmux(context.Background(), c.tmuxArgs,
			"send-keys", "-l", "-t", c.pane, "--", string(p[off:end])); err != nil {
			if written > 0 {
				return written, fmt.Errorf("tmux: send-keys: %w", err)
			}
			return 0, fmt.Errorf("tmux: send-keys: %w", err)
		}
		written += end - off
		off = end
	}
	return written, nil
}

// Resize implements [channel.Channel]. Resizes the WINDOW (which
// containing pane belongs to) since pane resize without a window mode
// requires extra setup.
func (c *Channel) Resize(ctx context.Context, cols, rows uint16) error {
	if c.closed.Load() {
		return channel.ErrChannelClosed
	}
	return runTmux(ctx, c.tmuxArgs,
		"resize-window", "-t", c.pane,
		"-x", fmt.Sprint(cols),
		"-y", fmt.Sprint(rows),
	)
}

// Caps implements [channel.Channel].
func (c *Channel) Caps() channel.Caps {
	return channel.Caps{
		BinarySafe:        false,
		SeparateStderr:    false,
		ScrollbackLimited: false, // pipe-pane streams every byte
		MaxWriteChunk:     c.chunkSize,
	}
}

// Close implements [channel.Channel]. Stops pipe-pane and removes the
// temp logfile if Options.LogFile was empty.
func (c *Channel) Close() error {
	var firstErr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.doneCh)

		// Stop pipe-pane (no command argument disables piping).
		// Best-effort — the pane may already be gone.
		_ = runTmux(context.Background(), c.tmuxArgs, "pipe-pane", "-t", c.pane)

		if err := c.logFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		if c.logIsTemp {
			_ = os.Remove(c.logPath)
		}
	})
	return firstErr
}

// LogFile returns the path the channel reads from. Useful for
// integration tests and post-mortem inspection.
func (c *Channel) LogFile() string { return c.logPath }

var _ channel.Channel = (*Channel)(nil)

// runTmux execs tmux with the given args, returning any non-nil
// combined output as part of the error message.
func runTmux(ctx context.Context, prefix []string, args ...string) error {
	full := append(append([]string{}, prefix...), args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: tmux %v: %s", err, full, bytes.TrimSpace(out))
	}
	return nil
}

func socketArgs(socket string, isPath bool) []string {
	if socket == "" {
		return nil
	}
	if isPath {
		return []string{"-S", socket}
	}
	return []string{"-L", socket}
}

func resolveLogPath(requested string) (path string, isTemp bool, err error) {
	if requested != "" {
		return requested, false, nil
	}
	f, err := os.CreateTemp("", "ptyrelay-tmux-*.log")
	if err != nil {
		return "", false, err
	}
	path = f.Name()
	_ = f.Close()
	return path, true, nil
}

// shellSingleQuote is the minimal POSIX quote needed for the pipe-pane
// shell command argument. We intentionally don't import
// internal/shellquote here — Channel implementations should not depend
// on backend utilities.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
