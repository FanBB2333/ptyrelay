// Package websocket provides a generic [channel.Channel] backed by a single
// WebSocket connection (RFC 6455).
//
// The default behavior is "raw bytes in binary frames": each Write produces
// one binary WebSocket frame, each received frame's payload is queued for
// Read. This matches the simplest stdio-over-WS bridges (a thin shim around
// `socat`, `wetty -p`, ad-hoc proxies) and is enough for ptyrelay's Session
// layer to ride on top.
//
// Servers that wrap their stream in an envelope — ttyd's leading
// '0' /'1' byte for input/resize, code-local's JSON message — supply
// Options.Encode / Options.Decode / Options.EncodeResize hooks. The Channel
// stays oblivious; only the hooks know the envelope.
package websocket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/channel"
	gws "github.com/gorilla/websocket"
)

// Message types, re-exported so Encode/Decode hooks don't need to import
// gorilla directly.
const (
	TextMessage   = gws.TextMessage
	BinaryMessage = gws.BinaryMessage
)

// Options configures [Dial].
type Options struct {
	// URL is the ws:// or wss:// endpoint. Required.
	URL string

	// Header carries optional headers for the upgrade handshake
	// (auth tokens, Origin, Sec-WebSocket-Protocol, etc.).
	Header http.Header

	// DialTimeout caps the upgrade handshake. Default: 10s.
	DialTimeout time.Duration

	// DialRetries is the number of additional dial attempts after the
	// first one fails on a transport error (refused connection,
	// timeout, transient DNS). 0 means "no retry, fail fast".
	//
	// HTTP-level rejections (401, 403, 404 on the WS upgrade) are
	// surfaced immediately without retrying — they indicate a
	// configuration problem, not a transient failure.
	DialRetries int

	// DialBackoff is the base wait between retries; each attempt
	// doubles the previous wait (`backoff << attempt`). 0 means 200ms.
	DialBackoff time.Duration

	// PingInterval, when > 0, enables WebSocket-level keepalive: a
	// PingMessage is sent every PingInterval, and the read deadline
	// is extended on every Pong. If the peer goes ~3×PingInterval
	// without responding, ReadMessage fails and the Channel surfaces
	// the error as a torn connection — much better than a half-open
	// TCP socket where Read just blocks forever.
	//
	// Recommended starting point: 30s. Set to 0 (default) to opt out.
	PingInterval time.Duration

	// PongTimeout caps how long after a Ping we wait for a Pong
	// before considering the connection dead. Only meaningful when
	// PingInterval > 0. Default: 3 × PingInterval.
	PongTimeout time.Duration

	// Encode optionally wraps a Write payload into a frame body and picks
	// the WebSocket message type. nil means "send the bytes verbatim as a
	// binary frame". This is the hook that ttyd / code-local adapters use
	// to prepend their per-message envelope.
	Encode func(payload []byte) (body []byte, messageType int, err error)

	// Decode optionally extracts payload bytes from a received frame.
	// nil means "treat the frame body verbatim as bytes" — both binary
	// and text frames are accepted as raw bytes. messageType is one of
	// TextMessage / BinaryMessage.
	Decode func(messageType int, body []byte) (payload []byte, err error)

	// EncodeResize optionally builds a resize-control frame. nil means
	// Channel.Resize is a no-op (suitable when geometry is irrelevant
	// or negotiated out-of-band).
	EncodeResize func(cols, rows uint16) (body []byte, messageType int, err error)

	// Reconnect enables transparent re-Dial when the underlying TCP
	// drops mid-session. Semantics are explicitly NOT magic:
	//
	//   - The in-flight Read returns [ErrReconnected] once so the
	//     caller knows the byte stream is no longer continuous (any
	//     framing/sentinel parser sitting on top MUST reset).
	//   - Subsequent Reads/Writes use the fresh connection.
	//   - We retry up to MaxReconnects times with ReconnectBackoff
	//     spacing; exceeding the budget surfaces a sticky terminal
	//     error like the no-reconnect path.
	//
	// Reconnect at this layer cannot resurrect remote state: the new
	// TCP connection talks to a new ttyd/socat/agent process, with
	// fresh shell state. It's a building block, not a panacea. Higher
	// layers that own session state (the FramedSession sentinel
	// parser, an AgentBackend REPL) must observe ErrReconnected and
	// rebuild their state.
	Reconnect bool

	// MaxReconnects caps how many redials we attempt across the
	// Channel's lifetime. 0 means "no cap" — the Channel will keep
	// trying until Close. Negative is invalid (treated as 0).
	MaxReconnects int

	// ReconnectBackoff is the wait between successive reconnect
	// attempts. 0 → 500ms default. The dial itself still honors
	// DialTimeout / DialRetries / DialBackoff per attempt, so a
	// flaky redial loop benefits from both layers of retry.
	ReconnectBackoff time.Duration
}

// ErrReconnected is returned by [Channel.Read] exactly once after a
// successful mid-session reconnect, signalling that all buffered
// bytes from before the disconnect have been discarded and any
// frame/sentinel state must be reset. Subsequent Reads block on the
// fresh connection like normal.
var ErrReconnected = errors.New("websocket: connection re-established (stream discontinuity)")

// Channel is a [channel.Channel] backed by one WebSocket connection.
type Channel struct {
	conn *gws.Conn

	enc func([]byte) ([]byte, int, error)
	dec func(int, []byte) ([]byte, error)
	rs  func(uint16, uint16) ([]byte, int, error)

	opts Options // retained for Reconnect (URL, Header, timeouts, keepalive)

	writeMu sync.Mutex

	readMu                sync.Mutex
	cond                  *sync.Cond
	buf                   bytes.Buffer
	readErr               error // sticky; nil until pump exits with no reconnect path
	pendingReconnectErr   error // one-shot: drained by next Read
	reconnectsRemaining   int   // counter, only meaningful when Reconnect=true
	reconnectAttemptCount int   // for tests/telemetry
	closed                bool

	closeOnce sync.Once
	stopPing  chan struct{} // closed in Close to stop the ping ticker
}

// Dial opens a WebSocket connection per opts and returns a Channel ready
// for use. The caller must Close() the returned Channel.
//
// When DialRetries > 0, transient transport failures (refused, timeout,
// transient DNS) trigger up to that many additional attempts with
// exponential backoff. HTTP-level upgrade failures (4xx/5xx) are
// surfaced immediately on the first attempt — they indicate
// configuration, not flakiness, and silent retry would just paper over
// a real bug.
func Dial(ctx context.Context, opts Options) (*Channel, error) {
	if opts.URL == "" {
		return nil, errors.New("websocket: URL required")
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	backoff := opts.DialBackoff
	if backoff <= 0 {
		backoff = 200 * time.Millisecond
	}
	dialer := *gws.DefaultDialer
	dialer.HandshakeTimeout = timeout

	conn, err := dialWithRetry(ctx, opts, dialer, timeout, backoff)
	if err != nil {
		return nil, err
	}
	c := &Channel{
		conn:                conn,
		enc:                 opts.Encode,
		dec:                 opts.Decode,
		rs:                  opts.EncodeResize,
		opts:                opts,
		stopPing:            make(chan struct{}),
		reconnectsRemaining: opts.MaxReconnects,
	}
	c.cond = sync.NewCond(&c.readMu)
	c.wireConn()
	go c.readPump()
	return c, nil
}

// dialWithRetry performs the dial loop (handshake timeout + transient
// retry/backoff). Separated from Dial so reconnect can reuse it.
func dialWithRetry(
	ctx context.Context,
	opts Options,
	dialer gws.Dialer,
	timeout time.Duration,
	backoff time.Duration,
) (*gws.Conn, error) {
	var lastErr error
	for attempt := 0; attempt <= opts.DialRetries; attempt++ {
		dctx, cancel := context.WithTimeout(ctx, timeout)
		conn, _, err := dialer.DialContext(dctx, opts.URL, opts.Header)
		cancel()
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if !isRetryableDialError(err) {
			return nil, err
		}
		if attempt == opts.DialRetries {
			break
		}
		wait := backoff << attempt
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// wireConn applies per-connection setup that needs to happen both on
// the initial dial and after each successful reconnect: keepalive
// goroutine if PingInterval > 0.
//
// Read deadlines / pong handlers from the previous conn don't carry
// over — gws.Conn pointers are independent — so we re-wire from
// scratch each time.
func (c *Channel) wireConn() {
	if c.opts.PingInterval > 0 {
		pongTimeout := c.opts.PongTimeout
		if pongTimeout <= 0 {
			pongTimeout = 3 * c.opts.PingInterval
		}
		c.startKeepalive(c.opts.PingInterval, pongTimeout)
	}
}

// startKeepalive wires WebSocket-level ping/pong so a half-open TCP
// socket can't strand a Read forever. The mechanism has three parts:
//
//  1. A pong handler that bumps the read deadline by pongTimeout every
//     time the peer answers — proof of life resets the watchdog.
//  2. An initial SetReadDeadline so we don't have to receive a pong
//     before the first ping to start the clock.
//  3. A goroutine that sends a PingMessage every `interval`. It exits
//     when stopPing closes (Close()) or when a ping write fails (the
//     connection is gone and readPump will surface the error).
//
// We intentionally don't try to be clever about resetting the deadline
// on every received data frame: pongs alone are sufficient evidence,
// and tying the deadline to data would let a peer that sends junk
// keep a broken half-open link alive.
func (c *Channel) startKeepalive(interval, pongTimeout time.Duration) {
	// Capture conn at goroutine start. After a reconnect c.conn is
	// swapped to a new pointer; the next wireConn() spawns a fresh
	// keepalive on the new conn while this one exits on first ping
	// write failure.
	conn := c.conn
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stopPing:
				return
			case <-ticker.C:
				c.writeMu.Lock()
				err := conn.WriteControl(
					gws.PingMessage, nil,
					time.Now().Add(interval),
				)
				c.writeMu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
}

// isRetryableDialError says whether err is the kind of failure that's
// worth a second attempt. We treat anything from gws.ErrBadHandshake
// as terminal: the server reached us and rejected the upgrade, so
// hammering it harder doesn't help.
func isRetryableDialError(err error) bool {
	// gorilla returns ErrBadHandshake for HTTP-status upgrade failures
	// (401/403/404/etc.). Treat the whole error chain as terminal.
	if errors.Is(err, gws.ErrBadHandshake) {
		return false
	}
	return true
}

// readPump runs until the connection drops or Close is called. It pushes
// every decoded payload into c.buf and broadcasts to wake any blocked
// Read.
//
// On a connection error, if Reconnect is enabled and the remaining
// budget allows, the pump triggers a re-Dial via attemptReconnect and
// — on success — keeps running against the new conn. The first Read
// after the swap is woken with ErrReconnected so the caller can
// reset any framing state.
func (c *Channel) readPump() {
	for {
		conn := c.conn
		mt, body, err := conn.ReadMessage()
		c.readMu.Lock()
		if err != nil {
			if c.closed {
				c.readMu.Unlock()
				return
			}
			if c.opts.Reconnect && c.shouldTryReconnect() {
				c.readMu.Unlock()
				if c.attemptReconnect() {
					continue
				}
				c.readMu.Lock()
			}
			// Map a clean close to io.EOF so callers can use the
			// standard io.Reader idiom (`err == io.EOF`) for
			// end-of-stream.
			if gws.IsCloseError(err, gws.CloseNormalClosure, gws.CloseGoingAway) {
				c.readErr = io.EOF
			} else {
				c.readErr = err
			}
			c.cond.Broadcast()
			c.readMu.Unlock()
			return
		}
		payload := body
		if c.dec != nil {
			payload, err = c.dec(mt, body)
			if err != nil {
				c.readErr = err
				c.cond.Broadcast()
				c.readMu.Unlock()
				return
			}
		}
		if len(payload) > 0 {
			c.buf.Write(payload)
			c.cond.Broadcast()
		}
		c.readMu.Unlock()
	}
}

// shouldTryReconnect reports whether the reconnect budget allows
// another attempt. MaxReconnects == 0 is "no cap".
//
// Must be called with readMu held.
func (c *Channel) shouldTryReconnect() bool {
	if c.opts.MaxReconnects == 0 {
		return true
	}
	return c.reconnectsRemaining > 0
}

// attemptReconnect re-Dials per c.opts and, on success, swaps c.conn
// in under writeMu. Returns true if the pump should keep running.
// On failure, leaves c.conn pointing at the (dead) old conn and
// returns false so the pump can finalize the terminal readErr.
func (c *Channel) attemptReconnect() bool {
	backoff := c.opts.ReconnectBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	timeout := c.opts.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialBackoff := c.opts.DialBackoff
	if dialBackoff <= 0 {
		dialBackoff = 200 * time.Millisecond
	}
	dialer := *gws.DefaultDialer
	dialer.HandshakeTimeout = timeout

	// Wait before retry so we don't pin a CPU on a server that's
	// rebooting. ReconnectBackoff is on top of DialBackoff so callers
	// have two coarse-grained knobs.
	select {
	case <-c.stopPing:
		return false
	case <-time.After(backoff):
	}

	newConn, err := dialWithRetry(context.Background(), c.opts, dialer, timeout, dialBackoff)
	c.readMu.Lock()
	if c.closed {
		c.readMu.Unlock()
		if newConn != nil {
			_ = newConn.Close()
		}
		return false
	}
	c.reconnectAttemptCount++
	if err != nil {
		// Don't count failed attempts toward the budget; budget is
		// for *successful* reconnects which represent stream
		// discontinuities the caller has to absorb. Failure to
		// reconnect terminates the channel cleanly via the caller.
		c.readMu.Unlock()
		return false
	}
	if c.opts.MaxReconnects > 0 {
		c.reconnectsRemaining--
	}
	// Drop any buffered bytes from the old conn — framing context is
	// dead and surfacing stale bytes would corrupt the next op.
	c.buf.Reset()
	c.pendingReconnectErr = ErrReconnected

	c.writeMu.Lock()
	oldConn := c.conn
	c.conn = newConn
	c.writeMu.Unlock()

	c.cond.Broadcast()
	c.readMu.Unlock()

	_ = oldConn.Close()
	c.wireConn()
	return true
}

// Read drains buffered bytes, blocking when empty until either more
// arrive or the connection ends. After a successful mid-session
// reconnect, the next Read returns ([ErrReconnected], 0) exactly once
// to signal stream discontinuity; subsequent Reads proceed against
// the fresh connection.
func (c *Channel) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.buf.Len() == 0 && c.readErr == nil && c.pendingReconnectErr == nil && !c.closed {
		c.cond.Wait()
	}
	if c.buf.Len() > 0 {
		return c.buf.Read(b)
	}
	if c.pendingReconnectErr != nil {
		err := c.pendingReconnectErr
		c.pendingReconnectErr = nil
		return 0, err
	}
	if c.closed {
		return 0, channel.ErrChannelClosed
	}
	return 0, c.readErr
}

// Write encodes payload and emits one WebSocket frame.
func (c *Channel) Write(b []byte) (int, error) {
	body, mt := b, gws.BinaryMessage
	if c.enc != nil {
		var err error
		body, mt, err = c.enc(b)
		if err != nil {
			return 0, err
		}
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteMessage(mt, body); err != nil {
		return 0, err
	}
	return len(b), nil
}

// Resize sends a resize control frame if EncodeResize was configured;
// otherwise it is a no-op.
func (c *Channel) Resize(_ context.Context, cols, rows uint16) error {
	if c.rs == nil {
		return nil
	}
	body, mt, err := c.rs(cols, rows)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(mt, body)
}

// Close terminates the connection. Safe to call more than once; only the
// first call performs work.
func (c *Channel) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// Signal the keepalive goroutine before touching conn — once
		// it's stopped writing, our polite-close WriteControl won't
		// race with a concurrent ping.
		close(c.stopPing)
		c.readMu.Lock()
		c.closed = true
		c.cond.Broadcast()
		c.readMu.Unlock()
		// Best-effort polite close; readPump will observe the resulting
		// CloseError or hard EOF and exit.
		_ = c.conn.WriteControl(
			gws.CloseMessage,
			gws.FormatCloseMessage(gws.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
		err = c.conn.Close()
	})
	return err
}

// Caps returns the channel's capability set. WebSocket is binary-safe and
// has no per-message size limit imposed by the transport itself
// (gorilla's default read limit is 32 MiB and is configurable on Conn).
func (c *Channel) Caps() channel.Caps {
	return channel.Caps{
		BinarySafe:        true,
		SeparateStderr:    false,
		ScrollbackLimited: false,
		MaxWriteChunk:     0,
		Concurrent:        false,
	}
}

var _ channel.Channel = (*Channel)(nil)
