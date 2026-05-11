// Package agent implements [backend.Backend] by talking to a remote
// ptyrelay-agent over a [session.FramedSession].
//
// v0.2.0 ships the one-shot transport: each op spawns a fresh agent
// process via Session.RunFramed and exchanges one line-delimited JSON
// message in each direction. The REPL transport (much lower per-op
// latency) lands in M5c on top of Session.Pipe.
//
// Agents binaries are uploaded by the M6 bootstrap layer; this package
// just consumes a path it can `exec` on the remote.
package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/proto"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// Backend implements [backend.Backend] over an agent.
type Backend struct {
	sess      *session.FramedSession
	agentPath string
	mode      Mode
	log       *slog.Logger

	// Atomic counter used to generate per-request IDs. Helpful for
	// debugging traces and used by the REPL transport for response
	// correlation sanity checks.
	idCounter atomic.Uint64

	// REPL-mode state. nil unless mode==ModeREPL and a request has
	// been issued (lazy start). replInitMu guards the start handshake;
	// the per-op call serialization lives inside replState.
	replInitMu sync.Mutex
	repl       *replState
}

// Option customizes a Backend at construction.
type Option func(*Backend)

// WithLogger installs a structured logger. Nil (default) produces a
// silent backend. Events are attached with `backend=agent` plus the
// current mode (`one_shot` or `repl`).
func WithLogger(l *slog.Logger) Option {
	return func(b *Backend) {
		if l != nil {
			b.log = l.With("backend", "agent")
		}
	}
}

// New constructs an AgentBackend that invokes the agent at agentPath on
// the remote. agentPath should be an absolute path or a name that
// resolves on the remote's PATH; arguments are shell-quoted before
// execution.
func New(sess *session.FramedSession, agentPath string, opts ...Option) *Backend {
	b := &Backend{sess: sess, agentPath: agentPath, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// AgentPath returns the path the backend invokes. Test-only helper.
func (b *Backend) AgentPath() string { return b.agentPath }

// Probe runs `ping` and verifies the agent answers with a matching
// protocol version. Failure of any kind here means RouterBackend
// shouldn't try to use the agent.
func (b *Backend) Probe(ctx context.Context) error {
	mode := "one_shot"
	if b.mode == ModeREPL {
		mode = "repl"
	}
	b.log.DebugContext(ctx, "probe.start", "mode", mode, "agent_path", b.agentPath)
	start := time.Now()
	var data proto.PingData
	if err := b.callOp(ctx, proto.OpPing, nil, &data); err != nil {
		b.log.ErrorContext(ctx, "probe.done", "mode", mode,
			"duration_ms", time.Since(start).Milliseconds(), "err", err.Error())
		return fmt.Errorf("agent: probe: %w", err)
	}
	if data.Version != proto.Version {
		b.log.ErrorContext(ctx, "probe.done", "mode", mode,
			"err", "protocol_version_mismatch", "got", data.Version, "want", proto.Version)
		return fmt.Errorf("agent: protocol version %d (want %d)", data.Version, proto.Version)
	}
	b.log.DebugContext(ctx, "probe.done", "mode", mode,
		"duration_ms", time.Since(start).Milliseconds(),
		"agent_version", data.AgentVersion)
	return nil
}

// Close tears down the REPL agent (sending `bye`, waiting for exit,
// releasing the Session lease). One-shot mode has no long-lived state
// and Close is a no-op.
func (b *Backend) Close() error {
	if b.mode == ModeREPL {
		return b.closeREPL()
	}
	return nil
}

// requestChunkSize bounds how many bytes of base64 we put on a single
// shell command line when staging the request. macOS PTYs cap input at
// MAX_INPUT (~1024 bytes) per non-canonical write — going larger
// silently drops bytes and corrupts the request mid-flight. Each chunk
// is a separate RunFramed call, so the chunk size affects round-trips
// per op but not correctness.
const requestChunkSize = 512

// callOp dispatches based on the configured mode.
func (b *Backend) callOp(ctx context.Context, op proto.Op, args, out any) error {
	if b.mode == ModeREPL {
		return b.callREPL(ctx, op, args, out)
	}
	return b.callOneShot(ctx, op, args, out)
}

// callOneShot marshals args, stages the request on the remote via short
// `printf '%s' >> tmp` appends, then invokes the agent reading from
// that tempfile. The detour around stdin via a tempfile is what makes
// requests larger than ~1 KiB survive a macOS PTY hop.
func (b *Backend) callOneShot(ctx context.Context, op proto.Op, args, out any) error {
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

	var reqBuf bytes.Buffer
	if err := proto.WriteOneShot(&reqBuf, &req); err != nil {
		return fmt.Errorf("agent: encode request: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(reqBuf.Bytes())
	tmpPath := "/tmp/ptr-req." + id + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	tmpQ := shellquote.Quote(tmpPath)

	// Stage chunks. The first overwrites; subsequent chunks append.
	first := true
	for i := 0; i < len(encoded); i += requestChunkSize {
		end := i + requestChunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		redirect := ">"
		if !first {
			redirect = ">>"
		}
		stage := fmt.Sprintf("printf '%%s' %s %s %s",
			shellquote.Quote(encoded[i:end]), redirect, tmpQ)
		stageRes, err := b.sess.RunFramed(ctx, stage, nil)
		if err != nil {
			return fmt.Errorf("agent: stage chunk: %w", err)
		}
		if stageRes.ExitCode != 0 {
			return fmt.Errorf("agent: stage chunk exited %d: %s",
				stageRes.ExitCode, stageRes.Output)
		}
		first = false
	}

	// Empty request would mean we never wrote the file — guard.
	if first {
		stage := fmt.Sprintf(": > %s", tmpQ)
		if _, err := b.sess.RunFramed(ctx, stage, nil); err != nil {
			return fmt.Errorf("agent: stage empty: %w", err)
		}
	}

	// Invoke the agent reading from the staged file. The subshell
	// (`( ... )`) is essential — it isolates the trailing `exit`
	// from our long-lived session shell, so the agent's exit code
	// propagates without killing bash.
	cmd := fmt.Sprintf("( base64 -d < %s | %s --mode=one-shot; rc=$?; rm -f %s; exit $rc )",
		tmpQ, shellquote.Quote(b.agentPath), tmpQ)
	res, err := b.sess.RunFramed(ctx, cmd, nil)
	if err != nil {
		return fmt.Errorf("agent: run: %w", err)
	}
	if res.ExitCode != 0 {
		var resp proto.Response
		if perr := proto.ReadOneShot(bytes.NewReader(res.Output), &resp); perr == nil && !resp.OK {
			return mapErrKind(resp.ErrKind, errors.New(resp.Err))
		}
		return fmt.Errorf("agent: exited %d: %s", res.ExitCode, res.Output)
	}

	var resp proto.Response
	if err := proto.ReadOneShot(bytes.NewReader(res.Output), &resp); err != nil {
		return fmt.Errorf("agent: decode response: %w (body=%q)", err, res.Output)
	}
	if !resp.OK {
		return mapErrKind(resp.ErrKind, errors.New(resp.Err))
	}
	if out != nil && len(resp.Data) > 0 {
		if err := json.Unmarshal(resp.Data, out); err != nil {
			return fmt.Errorf("agent: decode data: %w", err)
		}
	}
	return nil
}

// mapErrKind translates the wire ErrKind to a typed Go error, wrapping
// os.ErrNotExist / os.ErrPermission so callers can `errors.Is(err,
// os.ErrNotExist)` transparently regardless of which backend produced
// the failure.
func mapErrKind(kind string, agentErr error) error {
	switch kind {
	case proto.ErrKindNotFound:
		return fmt.Errorf("agent: %s: %w", agentErr, os.ErrNotExist)
	case proto.ErrKindPermission:
		return fmt.Errorf("agent: %s: %w", agentErr, os.ErrPermission)
	case proto.ErrKindBadProto:
		return fmt.Errorf("agent: %s: %w", agentErr, backend.ErrTransient)
	case proto.ErrKindUnimplemented:
		return fmt.Errorf("agent: unimplemented: %w", agentErr)
	case proto.ErrKindUnknownOp:
		return fmt.Errorf("agent: unknown op: %w", agentErr)
	default:
		// Surface the underlying error so callers can match string
		// content if needed; classification was best-effort.
		return errors.New("agent: " + agentErr.Error())
	}
}
