// Package backend defines the high-level remote capability surface —
// RemoteFS + RemoteExec — that callers (Claude Code, etc.) consume.
//
// Two implementations live in subpackages:
//
//   - backend/shell.ShellBackend  — composes shell commands over a Session
//   - (later) backend/agent.AgentBackend  — speaks JSON-RPC to a remote Go binary
//
// A RouterBackend that probes for an agent and routes per-op-class is
// planned for v0.2.0.
package backend

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"time"
)

// Op identifies a remote operation by name. The constant set is shared
// across all Backend implementations so RouterBackend can dispatch
// uniformly.
type Op string

const (
	OpPing      Op = "ping"
	OpRead      Op = "read"
	OpWrite     Op = "write"
	OpStat      Op = "stat"
	OpLstat     Op = "lstat"
	OpList      Op = "list"
	OpRemove    Op = "remove"
	OpRename    Op = "rename"
	OpMkdirAll  Op = "mkdir_all"
	OpRun       Op = "run"
	OpOpenRead  Op = "open_read"
	OpOpenWrite Op = "open_write"
)

// OpClass describes the idempotency / safety profile of an Op.
//
// RouterBackend uses Class to decide auto-retry and auto-fallback policy:
//
//   - ReadOnly    — safe to retry, safe to auto-fallback to Shell
//   - Idempotent  — safe to retry (effect is the same), safe to fallback
//   - NonIdempotent — must NOT auto-retry; must NOT auto-fallback if the
//     op may have partially executed; surface the error to the caller
type OpClass int

const (
	ClassReadOnly OpClass = iota
	ClassIdempotent
	ClassNonIdempotent
)

// Class returns the idempotency class of op.
func (o Op) Class() OpClass {
	switch o {
	case OpPing, OpRead, OpStat, OpLstat, OpList, OpOpenRead:
		return ClassReadOnly
	case OpMkdirAll, OpRename, OpWrite, OpOpenWrite:
		// Write is "Idempotent*" — atomic write (tempfile + rename) is
		// safe to redo, but a partial dribble of bytes is not. The
		// ShellBackend implements this with tempfiles.
		return ClassIdempotent
	case OpRemove, OpRun:
		return ClassNonIdempotent
	default:
		return ClassNonIdempotent
	}
}

// FileInfo describes a file or directory entry.
//
// SymlinkTarget is populated only when IsSymlink is true and the underlying
// stat call returned the link target (Lstat does, Stat resolves through).
type FileInfo struct {
	Name          string
	Size          int64
	Mode          fs.FileMode
	ModTime       time.Time
	IsDir         bool
	IsSymlink     bool
	SymlinkTarget string
}

// Result is the outcome of a Run invocation.
//
// Stdout and Stderr are separated when the underlying transport supports it
// (Caps.SeparateStderr); on a PTY-backed Channel everything lands in Stdout
// and Stderr is empty.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Duration time.Duration
}

// RemoteFS is the file-system surface a Backend exposes.
//
// Idempotency classes are documented per method; see Op.Class for the
// machine-readable form RouterBackend consults.
type RemoteFS interface {
	// Read returns the entire contents of path. ReadOnly.
	//
	// For files larger than the backend's threshold, callers should use
	// OpenRead instead — Read may return ErrTooLarge.
	Read(ctx context.Context, path string) ([]byte, error)

	// Stat returns metadata for path, following symlinks. ReadOnly.
	Stat(ctx context.Context, path string) (*FileInfo, error)

	// Lstat returns metadata for path without following symlinks.
	// ReadOnly.
	Lstat(ctx context.Context, path string) (*FileInfo, error)

	// List returns the immediate children of path. ReadOnly.
	List(ctx context.Context, path string) ([]FileInfo, error)

	// OpenRead returns a streaming reader for path. ReadOnly.
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)

	// Write replaces path's contents atomically (tempfile + rename).
	// Idempotent: redoing Write produces the same end state.
	Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error

	// OpenWrite returns a streaming writer for path. The write is
	// finalized only when Close is called successfully; on error the
	// tempfile is cleaned up. Idempotent.
	OpenWrite(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error)

	// MkdirAll creates path and any missing parents. Idempotent.
	MkdirAll(ctx context.Context, path string, mode fs.FileMode) error

	// Rename renames oldPath to newPath. Idempotent (the resulting
	// state is the same regardless of how many times it succeeds; if
	// oldPath is gone after the first success, subsequent calls fail
	// deterministically).
	Rename(ctx context.Context, oldPath, newPath string) error

	// Remove removes path (file or empty directory). NonIdempotent in
	// the strict sense: a second Remove of the same path fails, and
	// callers should not blindly retry on transient transport errors.
	Remove(ctx context.Context, path string) error
}

// RemoteExec is the command-execution surface a Backend exposes.
type RemoteExec interface {
	// Run executes cmd on the remote and returns its result.
	//
	// stdin is delivered via a here-doc (in ShellBackend) or a JSON
	// payload (in AgentBackend). NonIdempotent — Run must not be
	// auto-retried.
	Run(ctx context.Context, cmd string, stdin []byte) (*Result, error)
}

// Backend composes RemoteFS and RemoteExec with a health probe.
type Backend interface {
	RemoteFS
	RemoteExec

	// Probe verifies the backend is reachable and the remote is in a
	// state we can talk to. ShellBackend probes by running a no-op echo
	// and checking framing health; AgentBackend pings the agent.
	Probe(ctx context.Context) error

	// Close releases resources. The underlying Session is NOT closed —
	// the Backend does not own it.
	Close() error
}

// ErrAgentMissing is returned by AgentBackend / RouterBackend when the
// remote agent binary is not present at the expected path.
var ErrAgentMissing = errors.New("backend: agent missing")

// ErrAgentDied is returned when the agent process terminated unexpectedly
// (parse error, process crash, EOF mid-response).
var ErrAgentDied = errors.New("backend: agent died")

// ErrTooLarge is returned by ShellBackend when an op would require moving
// more bytes than the configured threshold (default 4 MiB). Callers should
// fall back to OpenRead/OpenWrite or use AgentBackend.
var ErrTooLarge = errors.New("backend: payload exceeds shell threshold")

// ErrCorrupted is returned when a post-write checksum verification fails.
var ErrCorrupted = errors.New("backend: post-write checksum mismatch")

// ErrTransient signals a temporary failure suitable for retry on
// idempotent ops. RouterBackend's retry policy keys off this.
var ErrTransient = errors.New("backend: transient failure")
