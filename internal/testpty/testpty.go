// Package testpty provides a [channel.Channel] backed by a real PTY +
// shell subprocess, for integration tests that need PTY semantics
// (cooked-mode echo, line discipline, real flushing) the way production
// transports deliver them.
//
// It is test-only infrastructure; production code should not import it.
package testpty

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
	"github.com/creack/pty"
)

// PTYChannel adapts a creack/pty master file to channel.Channel.
type PTYChannel struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

// NewBash starts `bash --noprofile --norc -i` behind a fresh PTY and
// returns a Channel attached to its master side. t.Skip is called if
// bash is unavailable.
func NewBash(t *testing.T) *PTYChannel {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH")
	}
	cmd := exec.Command(bash, "--noprofile", "--norc", "-i")
	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start: %v", err)
	}
	return &PTYChannel{cmd: cmd, ptmx: ptmx}
}

// Read implements channel.Channel.
func (c *PTYChannel) Read(b []byte) (int, error) { return c.ptmx.Read(b) }

// Write implements channel.Channel.
func (c *PTYChannel) Write(b []byte) (int, error) { return c.ptmx.Write(b) }

// Resize implements channel.Channel; PTY geometry is currently a no-op
// for tests (the cooked-mode shell doesn't depend on it).
func (c *PTYChannel) Resize(_ context.Context, _, _ uint16) error { return nil }

// Caps implements channel.Channel.
func (c *PTYChannel) Caps() channel.Caps {
	return channel.Caps{
		BinarySafe:    false,
		MaxWriteChunk: 4096,
	}
}

// Close kills the subprocess and releases the PTY.
func (c *PTYChannel) Close() error {
	_ = c.ptmx.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

var _ channel.Channel = (*PTYChannel)(nil)
