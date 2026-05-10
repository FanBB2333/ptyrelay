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
}

// Channel is a [channel.Channel] backed by one WebSocket connection.
type Channel struct {
	conn *gws.Conn

	enc func([]byte) ([]byte, int, error)
	dec func(int, []byte) ([]byte, error)
	rs  func(uint16, uint16) ([]byte, int, error)

	writeMu sync.Mutex

	readMu  sync.Mutex
	cond    *sync.Cond
	buf     bytes.Buffer
	readErr error // sticky; nil until the read pump exits
	closed  bool

	closeOnce sync.Once
}

// Dial opens a WebSocket connection per opts and returns a Channel ready
// for use. The caller must Close() the returned Channel.
func Dial(ctx context.Context, opts Options) (*Channel, error) {
	if opts.URL == "" {
		return nil, errors.New("websocket: URL required")
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialer := *gws.DefaultDialer
	dialer.HandshakeTimeout = timeout

	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, _, err := dialer.DialContext(dctx, opts.URL, opts.Header)
	if err != nil {
		return nil, err
	}
	c := &Channel{
		conn: conn,
		enc:  opts.Encode,
		dec:  opts.Decode,
		rs:   opts.EncodeResize,
	}
	c.cond = sync.NewCond(&c.readMu)
	go c.readPump()
	return c, nil
}

// readPump runs until the connection drops or Close is called. It pushes
// every decoded payload into c.buf and broadcasts to wake any blocked Read.
func (c *Channel) readPump() {
	for {
		mt, body, err := c.conn.ReadMessage()
		c.readMu.Lock()
		if err != nil {
			// Map a clean close to io.EOF so callers can use the standard
			// io.Reader idiom (`err == io.EOF`) for end-of-stream.
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

// Read drains buffered bytes, blocking when empty until either more arrive
// or the connection ends.
func (c *Channel) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for c.buf.Len() == 0 && c.readErr == nil && !c.closed {
		c.cond.Wait()
	}
	if c.buf.Len() > 0 {
		return c.buf.Read(b)
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
