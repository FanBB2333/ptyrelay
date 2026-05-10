package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
)

// Default knobs. All overridable via Options. The values reflect typical
// SSH-over-tmux RTTs: a few hundred ms is fine for soft cancel; if even
// SIGQUIT takes more than a few seconds we should declare the channel
// dead rather than block forever.
const (
	defaultSoftCancelGrace = 2 * time.Second
	defaultHardCancelGrace = 5 * time.Second
	defaultMaxOutput       = 16 << 20 // 16 MiB
	defaultReadChunkSize   = 4096
)

// FramedSession turns a Channel into a serialized RPC channel using
// sentinel framing. It is the v0.1.0 [Session] implementation.
//
// FramedSession is safe to use from multiple goroutines: each RunFramed
// (or Pipe, once implemented) call acquires an internal mutex, so calls
// are serialized. This matches the contract — the underlying remote shell
// is single-threaded.
//
// FramedSession owns a single reader goroutine for the channel's
// lifetime. Sequential RunFramed calls share that reader, which avoids a
// race where a per-call goroutine could still be blocked in Channel.Read
// when the next call's goroutine starts and steal bytes destined for the
// new parser.
type FramedSession struct {
	ch    channel.Channel
	shell ShellKind

	mu sync.Mutex

	preluded bool
	dead     atomic.Bool

	softCancelGrace time.Duration
	hardCancelGrace time.Duration
	maxOutput       int
	readChunkSize   int

	// readCh receives bytes read from ch. Closed when the reader exits.
	readCh    chan readChunk
	readerCtx context.Context
	stopRead  context.CancelFunc

	// nonceFn is overridable for tests; production uses crypto/rand.
	nonceFn func() (string, error)
}

// Option customizes a FramedSession at construction.
type Option func(*FramedSession)

// WithSoftCancelGrace sets how long after the first Ctrl-C we wait before
// escalating to Ctrl-\\ (SIGQUIT). Default 2s.
func WithSoftCancelGrace(d time.Duration) Option {
	return func(s *FramedSession) { s.softCancelGrace = d }
}

// WithHardCancelGrace sets how long after Ctrl-\\ we wait before declaring
// the session dead. Default 5s.
func WithHardCancelGrace(d time.Duration) Option {
	return func(s *FramedSession) { s.hardCancelGrace = d }
}

// WithMaxOutput caps the bytes a single framed command may produce.
// Exceeding the cap fails the command with ErrProtocol; it does not kill
// the session. Default 16 MiB.
func WithMaxOutput(n int) Option {
	return func(s *FramedSession) { s.maxOutput = n }
}

// WithReadChunkSize sets the buffer size used for Channel reads. Default 4 KiB.
func WithReadChunkSize(n int) Option {
	return func(s *FramedSession) {
		if n > 0 {
			s.readChunkSize = n
		}
	}
}

// withNonceFn is for tests.
func withNonceFn(fn func() (string, error)) Option {
	return func(s *FramedSession) { s.nonceFn = fn }
}

// New constructs a FramedSession. The shell argument selects the prelude
// flavor; pass ShellSh if uncertain — it produces the most defensive
// snippet at the cost of leaving history enabled.
//
// New starts a background reader goroutine; Close releases it.
func New(ch channel.Channel, shell ShellKind, opts ...Option) *FramedSession {
	s := &FramedSession{
		ch:              ch,
		shell:           shell,
		softCancelGrace: defaultSoftCancelGrace,
		hardCancelGrace: defaultHardCancelGrace,
		maxOutput:       defaultMaxOutput,
		readChunkSize:   defaultReadChunkSize,
		nonceFn:         newNonce,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.readerCtx, s.stopRead = context.WithCancel(context.Background())
	s.readCh = make(chan readChunk, 32)
	go s.readerLoop()
	return s
}

func (s *FramedSession) readerLoop() {
	defer close(s.readCh)
	buf := make([]byte, s.readChunkSize)
	for {
		n, err := s.ch.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			select {
			case s.readCh <- readChunk{b: cp}:
			case <-s.readerCtx.Done():
				return
			}
		}
		if err != nil {
			select {
			case s.readCh <- readChunk{err: err}:
			case <-s.readerCtx.Done():
			}
			return
		}
	}
}

// RunFramed implements [Session].
func (s *FramedSession) RunFramed(ctx context.Context, cmd string, stdin []byte) (*Result, error) {
	if s.dead.Load() {
		return nil, ErrSessionDead
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.preluded {
		if err := s.runPreludeLocked(ctx); err != nil {
			return nil, fmt.Errorf("prelude: %w", err)
		}
		s.preluded = true
	}
	return s.runFramedLocked(ctx, cmd, stdin)
}

// Pipe implements [Session]. Streaming is a v0.2.0 feature (used by
// AgentBackend in REPL mode); v0.1.0 returns a stub error so existing
// callers fail loudly instead of hanging.
func (s *FramedSession) Pipe(_ context.Context, _ string) (io.WriteCloser, io.ReadCloser, <-chan PipeResult, error) {
	return nil, nil, nil, errors.New("session: Pipe not implemented in v0.1.0")
}

// Close implements [Session].
func (s *FramedSession) Close() error {
	s.dead.Store(true)
	if s.stopRead != nil {
		s.stopRead()
	}
	return s.ch.Close()
}

// Dead reports whether the session has been declared unrecoverable. Once
// true, every RunFramed returns ErrSessionDead.
func (s *FramedSession) Dead() bool {
	return s.dead.Load()
}

func (s *FramedSession) runPreludeLocked(ctx context.Context) error {
	// Run the prelude as a framed command. We don't care about its
	// output; we only need confirmation that it ran (exit code 0).
	res, err := s.runFramedLocked(ctx, Prelude(s.shell)+"; true", nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%w: prelude exited %d (output: %q)",
			ErrProtocol, res.ExitCode, truncateForError(res.Output))
	}
	return nil
}

func (s *FramedSession) runFramedLocked(ctx context.Context, cmd string, stdin []byte) (*Result, error) {
	nonce, err := s.nonceFn()
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	wrapped, err := wrapCommand(cmd, stdin, nonce)
	if err != nil {
		return nil, err
	}

	if err := s.writeAll([]byte(wrapped)); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}

	parser := newSentinelParser(nonce, s.maxOutput)
	return s.driveParser(ctx, parser)
}

// writeAll writes b to the channel, respecting Caps.MaxWriteChunk by
// splitting larger payloads into multiple Writes.
func (s *FramedSession) writeAll(b []byte) error {
	caps := s.ch.Caps()
	chunk := caps.MaxWriteChunk
	if chunk <= 0 || chunk >= len(b) {
		_, err := s.ch.Write(b)
		return err
	}
	for off := 0; off < len(b); {
		end := off + chunk
		if end > len(b) {
			end = len(b)
		}
		if _, err := s.ch.Write(b[off:end]); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// readChunk is the message a reader goroutine delivers to driveParser.
type readChunk struct {
	b   []byte
	err error
}

// driveParser reads from the session's shared reader, feeding bytes to
// the parser, until the parser is done or cancellation escalates to dead.
func (s *FramedSession) driveParser(ctx context.Context, parser *sentinelParser) (*Result, error) {
	start := time.Now()

	var (
		ctxDone     = ctx.Done()
		cancelStage int // 0 normal, 1 sent ^C, 2 sent ^\, 3 dead
		phaseTimer  *time.Timer
	)
	armPhase := func(d time.Duration) {
		if phaseTimer != nil {
			phaseTimer.Stop()
		}
		phaseTimer = time.NewTimer(d)
	}
	defer func() {
		if phaseTimer != nil {
			phaseTimer.Stop()
		}
	}()

	for {
		var phaseCh <-chan time.Time
		if phaseTimer != nil {
			phaseCh = phaseTimer.C
		}

		select {
		case rr, ok := <-s.readCh:
			if !ok {
				// Reader goroutine exited without reporting. Treat as
				// channel-closed.
				s.dead.Store(true)
				return nil, channel.ErrChannelClosed
			}
			if rr.err != nil {
				if errors.Is(rr.err, io.EOF) {
					s.dead.Store(true)
					return nil, fmt.Errorf("%w: %v", channel.ErrChannelClosed, rr.err)
				}
				s.dead.Store(true)
				return nil, fmt.Errorf("read: %w", rr.err)
			}
			done, perr := parser.feed(rr.b)
			if perr != nil {
				return nil, perr
			}
			if done {
				result := &Result{
					Output:   parser.output,
					ExitCode: parser.exitCode,
					Duration: time.Since(start),
				}
				if cancelStage > 0 {
					return result, fmt.Errorf("%w (output captured)", ErrCanceled)
				}
				return result, nil
			}

		case <-ctxDone:
			ctxDone = nil // disable repeated firing
			if cancelStage == 0 {
				if _, werr := s.ch.Write([]byte{0x03}); werr != nil {
					s.dead.Store(true)
					return nil, fmt.Errorf("send ^C: %w", werr)
				}
				cancelStage = 1
				armPhase(s.softCancelGrace)
			}

		case <-phaseCh:
			switch cancelStage {
			case 1:
				if _, werr := s.ch.Write([]byte{0x1c}); werr != nil {
					s.dead.Store(true)
					return nil, fmt.Errorf("send ^\\: %w", werr)
				}
				cancelStage = 2
				armPhase(s.hardCancelGrace)
			case 2:
				s.dead.Store(true)
				return nil, fmt.Errorf("%w: drain after ^\\ exceeded %s",
					ErrSessionDead, s.hardCancelGrace)
			}
		}
	}
}

// wrapCommand returns the shell snippet that runs cmd between framing
// sentinels, optionally feeding stdin via a here-doc.
//
// The wrapper is constructed so that PTY echo of the command line cannot
// produce a byte sequence matching the markers we scan for: the nonce is
// inserted via shell variable substitution (`$__PR_N`), so the echoed
// line contains the literal `$__PR_N` while the runtime printf produces
// the substituted form.
func wrapCommand(cmd string, stdin []byte, nonce string) (string, error) {
	hdDelim := "__PR_STDIN_" + nonce + "__"
	if len(stdin) > 0 && bytes.Contains(stdin, []byte(hdDelim)) {
		return "", fmt.Errorf("%w: stdin contains here-doc delimiter", ErrProtocol)
	}

	var sb strings.Builder
	sb.WriteString("__PR_N=")
	sb.WriteString(nonce)
	sb.WriteString(`; printf '\n__PR_BEG_'$__PR_N'__\n'; { `)
	sb.WriteString(cmd)
	// Use "\n}" rather than "; }" to terminate the brace group: when
	// cmd itself ends with a newline (multi-line scripts), the
	// "; }" form becomes "...\n; }" — a leading-`;` syntax error.
	sb.WriteString("\n}")
	if len(stdin) > 0 {
		// Here-doc: must be terminated by the delimiter on a line of
		// its own. After the closing delimiter line, the next command
		// must start a fresh logical line — a leading `;` would be a
		// syntax error, so we omit it.
		sb.WriteString(" <<'")
		sb.WriteString(hdDelim)
		sb.WriteString("'\n")
		sb.Write(stdin)
		if stdin[len(stdin)-1] != '\n' {
			sb.WriteByte('\n')
		}
		sb.WriteString(hdDelim)
		sb.WriteByte('\n')
		sb.WriteString(`__PR_RC=$?; printf '\n__PR_END_'$__PR_N'__:%d\n' "$__PR_RC"`)
	} else {
		sb.WriteString(`; __PR_RC=$?; printf '\n__PR_END_'$__PR_N'__:%d\n' "$__PR_RC"`)
	}
	sb.WriteByte('\n')
	return sb.String(), nil
}

// newNonce generates an 8-byte hex nonce from crypto/rand.
func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func truncateForError(b []byte) []byte {
	const limit = 256
	if len(b) <= limit {
		return b
	}
	return b[:limit]
}
