package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
	"github.com/FanBB2333/ptyrelay/pkg/proto"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// Mode selects how AgentBackend invokes the remote agent.
type Mode int

const (
	// ModeOneShot runs the agent fresh per op via Session.RunFramed
	// + a base64-staged tempfile request. Robust on any PTY at the
	// cost of fork+exec per op (tens to hundreds of ms typically).
	ModeOneShot Mode = iota

	// ModeREPL runs a long-lived agent via Session.Pipe; per-op
	// latency is bounded by JSON encode + transport, typically a
	// few ms. Holds an exclusive lease on the underlying Session
	// for the backend's lifetime — RunFramed and other Pipe calls
	// block until the backend is Close'd.
	ModeREPL
)

// WithMode picks the transport. Default is [ModeOneShot].
func WithMode(m Mode) Option {
	return func(b *Backend) { b.mode = m }
}

// replState holds the long-lived REPL session machinery. nil unless
// the backend is in REPL mode and has started.
type replState struct {
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	resultCh <-chan session.PipeResult

	// dec decodes line-delimited JSON responses from the agent's
	// stdout. We use a single decoder for the lifetime of the REPL —
	// json.NewDecoder buffers internally and must not be reset
	// between calls.
	dec *json.Decoder

	// callMu serializes callREPL invocations. The agent processes
	// requests sequentially, so concurrent callREPL calls would
	// interleave responses.
	callMu sync.Mutex

	// closed flips to true once `bye` has been delivered (or the
	// pipe died). Subsequent ops fail fast.
	closed bool
}

// startREPL launches the agent process via Session.Pipe, exchanges a
// warmup ping to confirm the agent is up and reading, and stashes the
// resulting streams. Called lazily on the first REPL op.
//
// The warmup ping is essential: without it, a large first request can
// arrive at the PTY's input buffer faster than bash can fork+exec the
// agent, blowing past macOS's MAX_INPUT and corrupting bytes mid-flight.
// One small round trip ensures the agent is reading before any
// user-supplied payload starts streaming in.
func (b *Backend) startREPL(ctx context.Context) error {
	cmd := shellquote.Quote(b.agentPath) + " --mode=repl"
	stdin, stdout, resultCh, err := b.sess.Pipe(ctx, cmd)
	if err != nil {
		return fmt.Errorf("agent: start REPL: %w", err)
	}
	b.repl = &replState{
		stdin:    stdin,
		stdout:   stdout,
		resultCh: resultCh,
		dec:      json.NewDecoder(stdout),
	}
	// Internal warmup — bypasses callMu since we hold replInitMu and
	// no other goroutine has access to b.repl yet.
	pingReq := proto.Request{
		V:  proto.Version,
		ID: "_warmup",
		Op: proto.OpPing,
	}
	if err := proto.WriteOneShot(b.repl.stdin, &pingReq); err != nil {
		b.repl = nil
		return fmt.Errorf("agent: warmup write: %w", err)
	}
	var resp proto.Response
	if err := b.repl.dec.Decode(&resp); err != nil {
		b.repl = nil
		return fmt.Errorf("agent: warmup read: %w", err)
	}
	if !resp.OK {
		b.repl = nil
		return fmt.Errorf("agent: warmup ping failed: %s", resp.Err)
	}
	return nil
}

// callREPL is the REPL-mode equivalent of callOp.
func (b *Backend) callREPL(ctx context.Context, op proto.Op, args, out any) error {
	b.replInitMu.Lock()
	if b.repl == nil {
		if err := b.startREPL(ctx); err != nil {
			b.replInitMu.Unlock()
			return err
		}
	}
	repl := b.repl
	b.replInitMu.Unlock()

	repl.callMu.Lock()
	defer repl.callMu.Unlock()

	if repl.closed {
		return errors.New("agent: REPL closed")
	}

	id := strconv.FormatUint(b.idCounter.Add(1), 16)
	var argsRaw json.RawMessage
	if args != nil {
		raw, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("agent: marshal args: %w", err)
		}
		argsRaw = raw
	}
	req := proto.Request{V: proto.Version, ID: id, Op: op, Args: argsRaw}

	if err := proto.WriteOneShot(repl.stdin, &req); err != nil {
		// Write failures usually mean the agent died; mark closed
		// so subsequent ops fail fast rather than hanging on the
		// next read.
		repl.closed = true
		return fmt.Errorf("agent: REPL write: %w", err)
	}

	var resp proto.Response
	if err := repl.dec.Decode(&resp); err != nil {
		repl.closed = true
		return fmt.Errorf("agent: REPL read: %w", err)
	}
	if resp.ID != "" && resp.ID != id {
		// Sanity check — since callREPL is mutex-serialized the
		// agent's responses are FIFO with our requests, but if a
		// future contributor parallelizes us this guard catches it.
		return fmt.Errorf("agent: REPL response id mismatch: got %q want %q", resp.ID, id)
	}
	if !resp.OK {
		return mapErrKind(resp.ErrKind, errors.New(resp.Err))
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("agent: REPL decode data: %w", err)
		}
	}
	return nil
}

// closeREPL sends `bye`, waits for the agent to exit, and releases
// the Session lease.
func (b *Backend) closeREPL() error {
	b.replInitMu.Lock()
	repl := b.repl
	b.repl = nil
	b.replInitMu.Unlock()

	if repl == nil {
		return nil
	}

	repl.callMu.Lock()
	if !repl.closed {
		// Best-effort bye. If it fails, the pipe is probably already
		// dying; we still wait for the result channel below.
		_ = proto.WriteOneShot(repl.stdin, &proto.Request{
			V: proto.Version, Op: proto.OpBye,
		})
		var resp proto.Response
		_ = repl.dec.Decode(&resp)
		repl.closed = true
	}
	repl.callMu.Unlock()

	_ = repl.stdin.Close()
	// Drain remaining output so the io.PipeWriter inside Session
	// doesn't deadlock the pump goroutine.
	go io.Copy(io.Discard, repl.stdout)
	res := <-repl.resultCh
	if res.Err != nil && !errors.Is(res.Err, io.EOF) {
		return res.Err
	}
	return nil
}
