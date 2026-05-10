// Package session defines the framing contract that turns a raw Channel
// into a request/response RPC channel.
//
// A Session wraps a Channel and uses sentinel framing (see RunFramed) to
// extract a single command's output from a continuous PTY byte stream. It
// is the layer where shell-specific concerns live: the prelude that mutes
// echo and locale, the cancellation escalation chain, and the parser that
// recovers exit codes and clean output bytes from cooked-mode noise.
package session

import (
	"context"
	"errors"
	"io"
	"time"
)

// Result is what a single framed command produced.
//
// Output contains the bytes between the BEG and END sentinels, after
// echo/CR/ANSI cleanup. Stderr is merged into Output on PTY-backed channels
// (that's the nature of a single byte stream); a future Channel that exposes
// SeparateStderr=true would surface it on its own stream — the Session API
// will grow then.
type Result struct {
	Output   []byte
	ExitCode int
	Duration time.Duration
}

// PipeResult is the terminal status of a streaming command. It is delivered
// once over the channel returned by Pipe, after the remote command exits.
type PipeResult struct {
	ExitCode int
	Err      error
}

// Session runs commands over a Channel with sentinel framing.
//
// A Session must serialize calls internally — concurrent RunFramed/Pipe
// calls on the same Session are not safe.
type Session interface {
	// RunFramed sends cmd, captures its output between sentinels, and
	// returns when the remote shell exits the wrapper.
	//
	// stdin, if non-nil, is delivered to the command via a here-doc; the
	// here-doc terminator is generated to avoid colliding with the
	// session's framing sentinels.
	RunFramed(ctx context.Context, cmd string, stdin []byte) (*Result, error)

	// Pipe runs cmd in streaming mode, returning writers/readers for
	// stdin and stdout plus a single-shot result channel. Caller must
	// Close the writer to signal EOF, and drain the reader until the
	// result channel fires.
	//
	// Pipe takes an exclusive lease on the Session — RunFramed and other
	// Pipes will block until the returned writer is closed and the
	// result channel has fired.
	Pipe(ctx context.Context, cmd string) (stdin io.WriteCloser, stdout io.ReadCloser, result <-chan PipeResult, err error)

	// Close releases the Session and the underlying Channel.
	Close() error
}

// ErrTimeout is returned when an operation exceeds its deadline.
//
// It wraps context.DeadlineExceeded; callers may use errors.Is to test
// either form.
var ErrTimeout = errors.New("session: timeout")

// ErrCanceled is returned when an operation is canceled via context.
//
// It wraps context.Canceled; callers may use errors.Is to test either form.
var ErrCanceled = errors.New("session: canceled")

// ErrProtocol is returned when sentinel framing fails — typically because
// the underlying shell produced output that violates the expected shape
// (missing END sentinel, malformed exit code, etc.).
var ErrProtocol = errors.New("session: protocol violation")

// ErrSessionDead is returned after the Session has decided the underlying
// Channel can no longer make forward progress (e.g. cancellation drain
// exceeded the hard timeout). Callers must construct a new Session.
var ErrSessionDead = errors.New("session: dead")
