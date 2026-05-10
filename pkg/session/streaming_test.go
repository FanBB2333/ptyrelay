package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
)

func TestStreamingParser_Basic(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newStreamingParser("ab12", &out)
	stream := []byte("noise\n__PR_BEG_ab12__\nhello world\n__PR_END_ab12__:0\n")
	done, err := p.feed(stream)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("expected done")
	}
	if got := out.String(); got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
	if p.exitCode != 0 {
		t.Errorf("exit %d", p.exitCode)
	}
}

func TestStreamingParser_DripFeed(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newStreamingParser("ab12", &out)
	stream := []byte("__PR_BEG_ab12__\nhello world\n__PR_END_ab12__:7\n")
	for i := 0; i < len(stream); i++ {
		done, err := p.feed(stream[i : i+1])
		if err != nil {
			t.Fatalf("byte %d: %v", i, err)
		}
		if done {
			break
		}
	}
	// In drip-fed mode the wrapper's `\n` flushes promptly (see the
	// type-level doc on streamingParser), so the user stream picks
	// up one extra `\n` compared to bulk-fed mode. Documented and
	// tolerated for the v0.2.0 REPL consumer.
	if got := out.String(); got != "hello world\n" {
		t.Errorf("got %q, want %q", got, "hello world\n")
	}
	if p.exitCode != 7 {
		t.Errorf("exit %d", p.exitCode)
	}
}

func TestStreamingParser_LargeFlushed(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newStreamingParser("ab12", &out)
	body := strings.Repeat("X", 64*1024)
	stream := []byte("__PR_BEG_ab12__\n" + body + "\n__PR_END_ab12__:0\n")
	if _, err := p.feed(stream); err != nil {
		t.Fatal(err)
	}
	if out.Len() != len(body) {
		t.Errorf("got %d bytes, want %d", out.Len(), len(body))
	}
}

// pipeWriterAdapter exposes io.Writer atop an io.PipeWriter for the
// test below — io.PipeWriter is already an io.Writer, but having a
// distinct type makes the intent clear.
type pipeWriterAdapter = io.PipeWriter

func TestPipe_BashReadEcho(t *testing.T) {
	t.Parallel()
	ch := testpty.NewBash(t)
	sess := New(ch, ShellBash,
		WithSoftCancelGrace(500*time.Millisecond),
		WithHardCancelGrace(500*time.Millisecond),
	)
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// `read` reads exactly one line from stdin then exits — natural
	// termination without needing VEOF (which the prelude's
	// `stty -icanon` would have disabled anyway).
	stdin, stdout, result, err := sess.Pipe(ctx, `read line; printf 'got: %s' "$line"`)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}

	// Drain stdout in a goroutine BEFORE writing — the pipe pump
	// flushes bytes through an io.Pipe; with no reader it blocks
	// indefinitely.
	var got bytes.Buffer
	doneRead := make(chan struct{})
	go func() {
		_, _ = io.Copy(&got, stdout)
		close(doneRead)
	}()

	if _, err := stdin.Write([]byte("hello pipe\n")); err != nil {
		t.Fatalf("stdin.Write: %v", err)
	}

	select {
	case res := <-result:
		if res.Err != nil {
			t.Fatalf("PipeResult err: %v", res.Err)
		}
		if res.ExitCode != 0 {
			t.Errorf("ExitCode=%d", res.ExitCode)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("timeout waiting for PipeResult")
	}

	<-doneRead
	if !strings.Contains(got.String(), "got: hello pipe") {
		t.Errorf("output missing 'got: hello pipe': %q", got.String())
	}
}

func TestPipe_SerializesWithRunFramed(t *testing.T) {
	t.Parallel()
	ch := testpty.NewBash(t)
	sess := New(ch, ShellBash)
	defer sess.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run a normal command first so prelude is done.
	if _, err := sess.RunFramed(ctx, "true", nil); err != nil {
		t.Fatalf("warm-up RunFramed: %v", err)
	}

	// Open a pipe and immediately try a RunFramed in another
	// goroutine; it must block on s.mu until the pipe goroutine
	// finishes.
	stdin, stdout, result, err := sess.Pipe(ctx, `read line; printf 'got: %s' "$line"`)
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	// Drain stdout so the pipe pump's PipeWriter doesn't deadlock.
	go io.Copy(io.Discard, stdout)

	runDone := make(chan error, 1)
	go func() {
		_, err := sess.RunFramed(ctx, "echo serialized", nil)
		runDone <- err
	}()

	// Give the goroutine a moment to attempt the call.
	select {
	case <-runDone:
		t.Fatal("RunFramed completed while Pipe was active")
	case <-time.After(100 * time.Millisecond):
	}

	// End the pipe by feeding the line `read` is waiting for.
	if _, err := stdin.Write([]byte("done\n")); err != nil {
		t.Fatal(err)
	}
	res := <-result
	if res.Err != nil {
		t.Fatalf("PipeResult: %v", res.Err)
	}

	// Now RunFramed should complete.
	select {
	case err := <-runDone:
		if err != nil {
			t.Errorf("RunFramed after Pipe: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunFramed did not unblock after Pipe finished")
	}
}

func TestPipe_DeadSessionRejected(t *testing.T) {
	t.Parallel()
	ch := testpty.NewBash(t)
	sess := New(ch, ShellBash)
	_ = sess.Close()
	_, _, _, err := sess.Pipe(context.Background(), "cat")
	if !errors.Is(err, ErrSessionDead) {
		t.Errorf("err=%v, want ErrSessionDead", err)
	}
}
