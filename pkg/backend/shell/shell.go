// Package shell implements [backend.Backend] by composing shell commands
// over a [session.Session].
//
// ShellBackend is the v0.1.0 default — it works against any remote with a
// POSIX shell, base64, and a small set of standard tools. It is also the
// bootstrap path: an AgentBackend (v0.2.0) is uploaded by reusing
// ShellBackend's Write to drop the binary onto the remote.
//
// All file ops are atomic where possible: writes go to a tempfile, get
// chmodded, are sha256-verified, and only then renamed into place. Reads
// of files larger than [Option] MaxShellFileSize return ErrTooLarge —
// callers should switch to OpenRead/OpenWrite (streaming) for those.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// Default knobs. Override via [Option].
const (
	// defaultChunkSize is the base64-encoded payload size per shell
	// command — chosen so the resulting command line stays well under
	// typical ARG_MAX (~256 KiB on Linux, ~1 MiB on macOS) and the
	// channel's MaxWriteChunk.
	defaultChunkSize = 32 * 1024

	// defaultMaxShellFileSize caps a single Read/Write op to keep
	// per-op latency bounded. Larger files must use streaming or wait
	// for AgentBackend.
	defaultMaxShellFileSize = 4 * 1024 * 1024
)

// Backend implements [backend.Backend] over a [session.FramedSession].
type Backend struct {
	sess *session.FramedSession
	log  *slog.Logger

	mu         sync.Mutex
	probed     bool
	probe      *probeResult
	closedFlag bool

	chunkSize        int
	maxShellFileSize int
}

// Option customizes a Backend at construction.
type Option func(*Backend)

// WithChunkSize overrides the per-op base64 chunk size. Larger values
// reduce round trips but risk hitting ARG_MAX or channel write limits;
// stay under ~64 KiB to be safe across BSD and Linux.
func WithChunkSize(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.chunkSize = n
		}
	}
}

// WithMaxShellFileSize overrides the threshold above which Read/Write
// return ErrTooLarge.
func WithMaxShellFileSize(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.maxShellFileSize = n
		}
	}
}

// WithLogger installs a structured logger. Nil (default) produces a
// silent backend; pass `slog.Default()` to send events to stderr, or
// a custom *slog.Logger to integrate with your application's logging.
//
// Events are attached with attr `backend=shell`. Logged events:
// `probe.start` (Debug), `probe.done` (Debug or Error), `op.start`
// (Debug, per RemoteFS/RemoteExec call), `op.done` (Debug or Error,
// with `duration_ms`).
func WithLogger(l *slog.Logger) Option {
	return func(b *Backend) {
		if l != nil {
			b.log = l.With("backend", "shell")
		}
	}
}

// New constructs a Backend. The first call that needs platform info
// triggers detection (cached); call Probe to do it eagerly.
func New(sess *session.FramedSession, opts ...Option) *Backend {
	b := &Backend{
		sess:             sess,
		log:              discardLogger(),
		chunkSize:        defaultChunkSize,
		maxShellFileSize: defaultMaxShellFileSize,
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// discardLogger returns a no-op slog.Logger. Used as the default so
// libraries that don't configure logging are silent.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Session returns the underlying FramedSession. Test-only helper —
// production code should not need it.
func (b *Backend) Session() *session.FramedSession { return b.sess }

// Probe runs (or re-runs) the platform/tool detection, replacing any
// cached result. Safe to call concurrently with FS/Exec methods — they
// will see whichever probe last completed.
func (b *Backend) Probe(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closedFlag {
		return errors.New("shell: backend closed")
	}
	b.log.DebugContext(ctx, "probe.start")
	start := time.Now()
	p, err := detect(ctx, b.sess)
	if err != nil {
		b.log.ErrorContext(ctx, "probe.done", "duration_ms", time.Since(start).Milliseconds(), "err", err.Error())
		return err
	}
	b.probe = p
	b.probed = true
	b.log.DebugContext(ctx, "probe.done", "duration_ms", time.Since(start).Milliseconds(),
		"os", p.OS, "stat_style", p.StatStyle)
	return nil
}

// Close marks the backend as unusable. It does NOT close the underlying
// session — the session is owned by the caller.
func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closedFlag = true
	return nil
}

// ensureProbed runs detection on first use.
func (b *Backend) ensureProbed(ctx context.Context) (*probeResult, error) {
	b.mu.Lock()
	if b.closedFlag {
		b.mu.Unlock()
		return nil, errors.New("shell: backend closed")
	}
	if b.probed {
		p := b.probe
		b.mu.Unlock()
		return p, nil
	}
	b.mu.Unlock()

	// Probe outside the lock to avoid blocking other callers — but
	// detect itself runs through the session mutex, which serializes.
	if err := b.Probe(ctx); err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	return b.probe, nil
}

// runShell wraps Session.RunFramed with a few cross-cutting concerns:
// no stdin shortcut and a uniform error wrapping for non-zero exits.
func (b *Backend) runShell(ctx context.Context, cmd string, stdin []byte) (*session.Result, error) {
	res, err := b.sess.RunFramed(ctx, cmd, stdin)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// shellError wraps a non-zero shell exit into a friendly error.
func shellError(op backend.Op, exit int, output []byte) error {
	return fmt.Errorf("shell: %s exited %d: %s", op, exit, output)
}
