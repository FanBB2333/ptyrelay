// Package channel defines the byte-stream transport abstraction that the
// Session layer rides on top of.
//
// A Channel is a bidirectional, ordered byte pipe (typically backed by a PTY,
// a tmux pane, or a WebSocket). It exposes its idiosyncrasies through Caps
// so the Session layer can adapt — chunking writes, deciding whether to rely
// on stderr separation, etc.
package channel

import (
	"context"
	"errors"
	"io"
)

// Caps describes the runtime properties of a Channel.
//
// Higher layers consult Caps before issuing IO that depends on these
// properties (e.g. binary safety, chunk size).
type Caps struct {
	// BinarySafe is true when arbitrary bytes — including NUL, ESC and
	// shell metacharacters — pass through unmodified end-to-end.
	BinarySafe bool

	// SeparateStderr is true when stderr arrives on a distinct stream
	// from stdout. PTY-backed channels merge them, so this is usually false.
	SeparateStderr bool

	// ScrollbackLimited is true when the channel's read side can drop
	// bytes if the consumer falls behind (e.g. tmux capture-pane). When
	// false, all bytes the remote produced are recoverable.
	ScrollbackLimited bool

	// MaxWriteChunk is the largest single Write the channel accepts
	// without truncation, or 0 for "no known limit". Callers must split
	// larger payloads themselves.
	MaxWriteChunk int

	// Concurrent is true when multiple goroutines may safely call Write
	// (or Read) on the same Channel without external locking. Most
	// transports require external serialization, so this is usually false.
	Concurrent bool
}

// Channel is the transport contract.
//
// Implementations must be safe for sequential use; concurrent use requires
// Caps.Concurrent.
type Channel interface {
	io.Reader
	io.Writer

	// Resize informs the remote of a new terminal geometry. Channels
	// without a notion of geometry (e.g. plain TCP) should return nil.
	Resize(ctx context.Context, cols, rows uint16) error

	// Close releases all resources. Subsequent Read/Write must return
	// ErrChannelClosed.
	Close() error

	// Caps returns the (immutable) capability set of this Channel.
	Caps() Caps
}

// ErrChannelClosed is returned from Read/Write/Resize after Close, or after
// the underlying transport has been observed to close cleanly.
var ErrChannelClosed = errors.New("channel: closed")

// ErrChannelDead is returned when the channel's transport has failed in a
// non-recoverable way (e.g. EPIPE on write, EOF mid-stream). Callers must
// not retry on the same Channel; they must construct a new one.
var ErrChannelDead = errors.New("channel: dead")
