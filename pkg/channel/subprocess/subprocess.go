// Package subprocess provides a generic [channel.Channel] that runs a
// local command and treats its stdio as the byte stream.
//
// This is the simplest possible adapter: anything that already speaks
// "stdin in, stdout out" — docker exec, kubectl exec, lxc exec, podman
// exec, ssh, socat — becomes a Channel without writing a transport.
//
// Recipes:
//
//	// docker exec -i <container> bash
//	ch, _ := subprocess.Start(ctx, subprocess.Options{
//	    Command: []string{"docker", "exec", "-i", "my-container", "bash"},
//	})
//
//	// kubectl exec -i -n <ns> <pod> -- bash
//	ch, _ := subprocess.Start(ctx, subprocess.Options{
//	    Command: []string{"kubectl", "exec", "-i", "-n", "prod", "api-0", "--", "bash"},
//	})
//
//	// Plain ssh, no tmux
//	ch, _ := subprocess.Start(ctx, subprocess.Options{
//	    Command: []string{"ssh", "-T", "user@host", "bash"},
//	})
//
// These all give you a fresh remote bash. Wrap with Session +
// ShellBackend exactly as you would for a tmux or WebSocket channel —
// the Backend stack is unchanged.
package subprocess

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
)

// Options configures [Start].
type Options struct {
	// Command is the argv to exec. Command[0] is looked up on PATH.
	// Required and must have length >= 1.
	Command []string

	// Env, if non-nil, fully replaces the child's environment. Default
	// (nil) inherits the parent's environ.
	Env []string

	// Dir is the working directory the child starts in. Empty means
	// inherit from the parent.
	Dir string

	// Caps overrides the default capability set. The default reports
	// BinarySafe=true and MaxWriteChunk=0, which is correct for the
	// canonical recipes (docker / kubectl / ssh -T pipe arbitrary
	// bytes through their stdio unchanged). Override if your specific
	// command imposes restrictions.
	Caps *channel.Caps
}

// Channel is a [channel.Channel] backed by one running subprocess.
type Channel struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	caps   channel.Caps

	closeOnce sync.Once
	closeErr  error
	exited    chan struct{}
}

// Start launches the subprocess and returns a Channel attached to its
// stdio. The caller must Close() to terminate the child.
//
// The child's stderr is discarded by default; if you need it, wrap with
// a custom writer in your invocation. (We don't promote stderr to the
// Channel byte stream because the Channel contract is "one ordered
// byte stream", and most consumers want stdout-only.)
func Start(ctx context.Context, opts Options) (*Channel, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("subprocess: Command is required")
	}

	cmd := exec.CommandContext(ctx, opts.Command[0], opts.Command[1:]...)
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	if opts.Dir != "" {
		cmd.Dir = opts.Dir
	}
	// New session: SIGINT to the parent process group doesn't kill
	// the child, and Close() can target the child's group cleanly.
	cmd.SysProcAttr = procAttr()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start %q: %w", opts.Command[0], err)
	}

	caps := channel.Caps{BinarySafe: true, MaxWriteChunk: 0}
	if opts.Caps != nil {
		caps = *opts.Caps
	}

	c := &Channel{
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		caps:   caps,
		exited: make(chan struct{}),
	}
	// Reap the child in the background so cmd.Wait races don't pile up.
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	return c, nil
}

// Read drains the child's stdout. Returns io.EOF when the child closes
// stdout (typically because it exited).
func (c *Channel) Read(p []byte) (int, error) { return c.stdout.Read(p) }

// Write delivers bytes to the child's stdin.
func (c *Channel) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Resize is a no-op: subprocess Channels don't have a notion of
// terminal geometry. Wrap in a PTY if you need that.
func (c *Channel) Resize(_ context.Context, _, _ uint16) error { return nil }

// Close terminates the child cleanly. Idempotent.
//
// Order matters: closing stdin first gives the child a chance to flush
// and exit on its own (`bash` exits on EOF on stdin); if it hasn't
// exited within a short grace, we signal SIGTERM and then SIGKILL.
func (c *Channel) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		// Wait a short grace for clean exit on EOF.
		select {
		case <-c.exited:
		case <-timerC(closeGrace):
			// Still alive — escalate.
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-c.exited:
				case <-timerC(killGrace):
					_ = c.cmd.Process.Kill()
					<-c.exited
				}
			}
		}
		_ = c.stdout.Close()
	})
	return c.closeErr
}

// Caps reports the configured capability set.
func (c *Channel) Caps() channel.Caps { return c.caps }

var _ channel.Channel = (*Channel)(nil)
