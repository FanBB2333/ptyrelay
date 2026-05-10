package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
	"github.com/creack/pty"
)

// ptyChannel adapts a creack/pty master file to channel.Channel. Tests
// drive a real shell behind a real PTY — the same shape as the production
// tmux/code-local environments, so PTY echo, line discipline and stdio
// flushing all behave realistically.
type ptyChannel struct {
	cmd  *exec.Cmd
	ptmx *os.File
	caps channel.Caps
}

func newBashPTYChannel(t *testing.T) *ptyChannel {
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
	return &ptyChannel{
		cmd:  cmd,
		ptmx: ptmx,
		caps: channel.Caps{BinarySafe: false, MaxWriteChunk: 4096},
	}
}

func (c *ptyChannel) Read(b []byte) (int, error)                  { return c.ptmx.Read(b) }
func (c *ptyChannel) Write(b []byte) (int, error)                 { return c.ptmx.Write(b) }
func (c *ptyChannel) Resize(_ context.Context, _, _ uint16) error { return nil }
func (c *ptyChannel) Caps() channel.Caps                          { return c.caps }
func (c *ptyChannel) Close() error {
	_ = c.ptmx.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	return nil
}

func TestFramedSession_BashEcho(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := sess.RunFramed(ctx, "echo hello", nil)
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	// The trailing newline `echo` emits is part of the user's output;
	// the framing layer preserves it (only the wrapper's own leading
	// "\n" before END is stripped).
	if string(res.Output) != "hello\n" {
		t.Errorf("output = %q, want %q", res.Output, "hello\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
}

func TestFramedSession_BashExitCode(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Subshell exit so the user's "exit" doesn't terminate our shell —
	// otherwise the wrapper's END printf never runs and we'd see an
	// EOF instead of an exit code.
	res, err := sess.RunFramed(ctx, "(exit 7)", nil)
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res.ExitCode)
	}
}

func TestFramedSession_BashSequential(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Verify the prelude only runs once: the first command sees it; the
	// second proves the session preserves shell state across calls.
	if _, err := sess.RunFramed(ctx, "X=hello", nil); err != nil {
		t.Fatalf("set X: %v", err)
	}
	res, err := sess.RunFramed(ctx, "echo $X world", nil)
	if err != nil {
		t.Fatalf("read X: %v", err)
	}
	if string(res.Output) != "hello world\n" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestFramedSession_BashStdin(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := sess.RunFramed(ctx, "cat -", []byte("piped data"))
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	// Here-doc syntax requires the body to end with a newline (the
	// delimiter must be on its own line), so wrapCommand appends one
	// if the user didn't. cat then echoes that final \n.
	if string(res.Output) != "piped data\n" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestFramedSession_BashEmptyOutput(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := sess.RunFramed(ctx, "true", nil)
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	if len(res.Output) != 0 {
		t.Errorf("output = %q, want empty", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
}

func TestFramedSession_BashMultilineOutput(t *testing.T) {
	t.Parallel()
	ch := newBashPTYChannel(t)
	sess := New(ch, ShellBash)
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := sess.RunFramed(ctx, "printf 'a\\nb\\nc\\n'", nil)
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	// The wrapper only strips ONE trailing newline (the one its own
	// printf emitted before the END marker). The trailing "\n" the
	// user emitted via printf '...\\n' is preserved.
	if string(res.Output) != "a\nb\nc\n" {
		t.Errorf("output = %q, want %q", res.Output, "a\nb\nc\n")
	}
}

// mockChannel is a controllable Channel for unit-testing the cancel state
// machine without a real shell.
type mockChannel struct {
	r io.Reader

	mu      sync.Mutex
	written bytes.Buffer
	closed  bool
}

func newMockChannel(r io.Reader) *mockChannel {
	return &mockChannel{r: r}
}

func (m *mockChannel) Read(b []byte) (int, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return 0, channel.ErrChannelClosed
	}
	return m.r.Read(b)
}

func (m *mockChannel) Write(b []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, channel.ErrChannelClosed
	}
	return m.written.Write(b)
}

func (m *mockChannel) Resize(_ context.Context, _, _ uint16) error { return nil }
func (m *mockChannel) Caps() channel.Caps {
	return channel.Caps{BinarySafe: true, MaxWriteChunk: 0}
}

func (m *mockChannel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockChannel) writtenSnapshot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.written.String()
}

// blockingReader reads block forever (until Read is poked by Close).
// Used so the session sees no output and the cancel chain has to
// escalate to dead.
type blockingReader struct {
	done chan struct{}
}

func newBlockingReader() *blockingReader {
	return &blockingReader{done: make(chan struct{})}
}

func (b *blockingReader) Read(_ []byte) (int, error) {
	<-b.done
	return 0, io.EOF
}

func (b *blockingReader) close() { close(b.done) }

func TestFramedSession_CancelEscalatesToDead(t *testing.T) {
	t.Parallel()

	br := newBlockingReader()
	defer br.close()
	mc := newMockChannel(br)

	sess := New(mc, ShellSh,
		WithSoftCancelGrace(30*time.Millisecond),
		WithHardCancelGrace(30*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := sess.RunFramed(ctx, "true", nil)
		errCh <- err
	}()

	// Give the session time to write the wrapped prelude before we
	// cancel — otherwise the cancel races the write and may surface as
	// a different error.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrSessionDead) {
			t.Errorf("err = %v, want ErrSessionDead", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunFramed did not return after cancel escalation")
	}

	written := mc.writtenSnapshot()
	if !strings.Contains(written, "\x03") {
		t.Errorf("Ctrl-C (\\x03) not sent; written=%q", written)
	}
	if !strings.Contains(written, "\x1c") {
		t.Errorf("Ctrl-\\ (\\x1c) not sent; written=%q", written)
	}
	if !sess.Dead() {
		t.Error("session should be marked dead")
	}
}

func TestFramedSession_DeadSessionRejectsCalls(t *testing.T) {
	t.Parallel()
	br := newBlockingReader()
	defer br.close()
	mc := newMockChannel(br)
	sess := New(mc, ShellSh)
	_ = sess.Close()
	_, err := sess.RunFramed(context.Background(), "true", nil)
	if !errors.Is(err, ErrSessionDead) {
		t.Errorf("err = %v, want ErrSessionDead", err)
	}
}

func TestWrapCommand_NonceSubstitution(t *testing.T) {
	t.Parallel()
	wrapped, err := wrapCommand("echo hi", nil, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper text itself must NOT contain the substituted form
	// "__PR_BEG_deadbeef__" — that's what guarantees PTY echo cannot
	// fool the parser. The substituted form only appears after the
	// shell expands $__PR_N at runtime.
	if strings.Contains(wrapped, "__PR_BEG_deadbeef__") {
		t.Errorf("wrapper leaks substituted BEG marker: %q", wrapped)
	}
	if strings.Contains(wrapped, "__PR_END_deadbeef__") {
		t.Errorf("wrapper leaks substituted END marker: %q", wrapped)
	}
	// The wrapper must contain the unsubstituted form so the shell can
	// build the marker at runtime.
	if !strings.Contains(wrapped, `__PR_BEG_'$__PR_N'__`) {
		t.Errorf("wrapper missing variable BEG marker: %q", wrapped)
	}
}

func TestWrapCommand_StdinDelimCollision(t *testing.T) {
	t.Parallel()
	stdin := []byte("normal\n__PR_STDIN_deadbeef__\nmore")
	_, err := wrapCommand("cat -", stdin, "deadbeef")
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
}
