// Package router implements [backend.Backend] by composing an
// [agent.Backend] (preferred for performance + binary safety + separated
// stderr) with a [shell.Backend] (always-available fallback).
//
// Routing rules:
//
//   - When the agent has been observed healthy, every op tries the
//     agent first.
//   - If an agent op fails AND the op is ReadOnly or Idempotent (per
//     [backend.Op.Class]), Router falls back to ShellBackend
//     transparently and marks the agent unhealthy. Subsequent ops skip
//     the agent and go straight to Shell, until the next successful
//     re-Probe.
//   - NonIdempotent ops never auto-fallback — Router surfaces the
//     agent error to the caller, who may have side-effects to consider
//     before retrying.
package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sync"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
)

// Backend is the routing backend. It implements [backend.Backend] and
// composes an agent + shell pair.
type Backend struct {
	agent *agent.Backend
	shell *shell.Backend
	log   *slog.Logger

	mu             sync.RWMutex
	agentHealthy   bool
	agentLastError error
}

// Option customizes a Backend at construction.
type Option func(*Backend)

// WithLogger installs a structured logger. Routing decisions and
// fallbacks are the highest-signal events here.
//
// Events emitted: `route` (Debug, per op, includes which backend was
// picked), `agent.unhealthy` (Warn, when an agent op fails and the
// op is fallbackable), `agent.healthy` (Info, on successful Probe).
func WithLogger(l *slog.Logger) Option {
	return func(b *Backend) {
		if l != nil {
			b.log = l.With("backend", "router")
		}
	}
}

// New constructs a RouterBackend. Both agent and shell must be ready —
// shell as a fallback always, agent as the preferred path.
//
// New does NOT probe automatically; call Probe (or any FS/Exec op,
// which probes lazily) before relying on the routing decision.
func New(a *agent.Backend, s *shell.Backend, opts ...Option) *Backend {
	b := &Backend{agent: a, shell: s, log: discardLogger()}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// discardLogger returns a no-op slog.Logger used as the default.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Agent / Shell expose the wrapped backends for tests + introspection.
func (b *Backend) Agent() *agent.Backend { return b.agent }
func (b *Backend) Shell() *shell.Backend { return b.shell }

// AgentHealthy reports whether the agent is currently considered
// usable. Test-only helper — production callers should not branch on
// this.
func (b *Backend) AgentHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.agentHealthy
}

// Probe re-probes the agent. ShellBackend's probe is run too — without
// it the fallback path can't function either.
func (b *Backend) Probe(ctx context.Context) error {
	if err := b.shell.Probe(ctx); err != nil {
		return fmt.Errorf("router: shell probe: %w", err)
	}
	if err := b.agent.Probe(ctx); err != nil {
		b.markAgentUnhealthy(err)
		b.log.WarnContext(ctx, "agent.unhealthy", "reason", "probe_failed", "err", err.Error())
		// We don't return error here: a router with a dead agent but
		// healthy shell is still functional, just slower. Callers
		// that want strict agent-mode should check AgentHealthy.
		return nil
	}
	b.markAgentHealthy()
	b.log.InfoContext(ctx, "agent.healthy")
	return nil
}

// Close closes both backends.
func (b *Backend) Close() error {
	var errs []error
	if err := b.agent.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := b.shell.Close(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (b *Backend) markAgentHealthy() {
	b.mu.Lock()
	b.agentHealthy = true
	b.agentLastError = nil
	b.mu.Unlock()
}

func (b *Backend) markAgentUnhealthy(err error) {
	b.mu.Lock()
	b.agentHealthy = false
	b.agentLastError = err
	b.mu.Unlock()
}

// ----- routing helpers -----

// route invokes fn under the routing rules: try agent first if healthy;
// fall back to shell for ReadOnly / Idempotent ops; never fall back for
// NonIdempotent ops. agentFn / shellFn are the per-backend callbacks.
//
// Generic helpers can't be unified across signatures because Go's
// generics don't extend cleanly across method-mismatched types — so
// each op method below calls route with closures.
func (b *Backend) route(
	ctx context.Context,
	op backend.Op,
	agentFn func() error,
	shellFn func() error,
) error {
	b.mu.RLock()
	healthy := b.agentHealthy
	b.mu.RUnlock()

	if healthy {
		b.log.DebugContext(ctx, "route", "op", string(op), "via", "agent")
		err := agentFn()
		if err == nil {
			return nil
		}
		if op.Class() == backend.ClassNonIdempotent {
			b.log.WarnContext(ctx, "agent.error.no_fallback",
				"op", string(op), "class", "non_idempotent", "err", err.Error())
			// Don't auto-fallback. Caller decides.
			return err
		}
		// Mark unhealthy and fall through to shell.
		b.markAgentUnhealthy(err)
		b.log.WarnContext(ctx, "agent.unhealthy", "reason", "op_failed",
			"op", string(op), "err", err.Error())
	}

	if shellFn == nil {
		// No shell counterpart available (e.g., a future agent-only
		// op).
		return errors.New("router: agent unavailable and op has no shell fallback")
	}
	b.log.DebugContext(ctx, "route", "op", string(op), "via", "shell")
	return shellFn()
}

// ----- RemoteFS -----

func (b *Backend) Read(ctx context.Context, path string) ([]byte, error) {
	var data []byte
	err := b.route(ctx, backend.OpRead,
		func() error {
			d, err := b.agent.Read(ctx, path)
			data = d
			return err
		},
		func() error {
			d, err := b.shell.Read(ctx, path)
			data = d
			return err
		},
	)
	return data, err
}

func (b *Backend) Write(ctx context.Context, path string, p []byte, mode fs.FileMode) error {
	return b.route(ctx, backend.OpWrite,
		func() error { return b.agent.Write(ctx, path, p, mode) },
		func() error { return b.shell.Write(ctx, path, p, mode) },
	)
}

func (b *Backend) Stat(ctx context.Context, path string) (*backend.FileInfo, error) {
	var info *backend.FileInfo
	err := b.route(ctx, backend.OpStat,
		func() error {
			i, err := b.agent.Stat(ctx, path)
			info = i
			return err
		},
		func() error {
			i, err := b.shell.Stat(ctx, path)
			info = i
			return err
		},
	)
	return info, err
}

func (b *Backend) Lstat(ctx context.Context, path string) (*backend.FileInfo, error) {
	var info *backend.FileInfo
	err := b.route(ctx, backend.OpLstat,
		func() error {
			i, err := b.agent.Lstat(ctx, path)
			info = i
			return err
		},
		func() error {
			i, err := b.shell.Lstat(ctx, path)
			info = i
			return err
		},
	)
	return info, err
}

func (b *Backend) List(ctx context.Context, path string) ([]backend.FileInfo, error) {
	var entries []backend.FileInfo
	err := b.route(ctx, backend.OpList,
		func() error {
			e, err := b.agent.List(ctx, path)
			entries = e
			return err
		},
		func() error {
			e, err := b.shell.List(ctx, path)
			entries = e
			return err
		},
	)
	return entries, err
}

func (b *Backend) MkdirAll(ctx context.Context, path string, mode fs.FileMode) error {
	return b.route(ctx, backend.OpMkdirAll,
		func() error { return b.agent.MkdirAll(ctx, path, mode) },
		func() error { return b.shell.MkdirAll(ctx, path, mode) },
	)
}

func (b *Backend) Rename(ctx context.Context, oldPath, newPath string) error {
	return b.route(ctx, backend.OpRename,
		func() error { return b.agent.Rename(ctx, oldPath, newPath) },
		func() error { return b.shell.Rename(ctx, oldPath, newPath) },
	)
}

func (b *Backend) Remove(ctx context.Context, path string) error {
	// NonIdempotent: route() refuses to auto-fallback.
	return b.route(ctx, backend.OpRemove,
		func() error { return b.agent.Remove(ctx, path) },
		func() error { return b.shell.Remove(ctx, path) },
	)
}

func (b *Backend) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := b.route(ctx, backend.OpOpenRead,
		func() error {
			r, err := b.agent.OpenRead(ctx, path)
			rc = r
			return err
		},
		func() error {
			r, err := b.shell.OpenRead(ctx, path)
			rc = r
			return err
		},
	)
	return rc, err
}

func (b *Backend) OpenWrite(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	var wc io.WriteCloser
	err := b.route(ctx, backend.OpOpenWrite,
		func() error {
			w, err := b.agent.OpenWrite(ctx, path, mode)
			wc = w
			return err
		},
		func() error {
			w, err := b.shell.OpenWrite(ctx, path, mode)
			wc = w
			return err
		},
	)
	return wc, err
}

// ----- RemoteExec -----

func (b *Backend) Run(ctx context.Context, cmd string, stdin []byte) (*backend.Result, error) {
	var res *backend.Result
	err := b.route(ctx, backend.OpRun,
		func() error {
			r, err := b.agent.Run(ctx, cmd, stdin)
			res = r
			return err
		},
		func() error {
			r, err := b.shell.Run(ctx, cmd, stdin)
			res = r
			return err
		},
	)
	return res, err
}

// Compile-time assertion.
var _ backend.Backend = (*Backend)(nil)
