package websocket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pwws "github.com/FanBB2333/ptyrelay/pkg/channel/websocket"
	gws "github.com/gorilla/websocket"
)

// startEcho stands up an httptest server that echoes every incoming
// WebSocket message back unchanged. Returns the ws:// URL and a cleanup.
func startEcho(t *testing.T, onResize func(int, []byte)) (string, *httptest.Server) {
	t.Helper()
	upgrader := gws.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			// Hook for tests that want to observe non-payload frames
			// (e.g. resize) sent by the client.
			if onResize != nil {
				onResize(mt, msg)
				continue
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv
}

func TestChannel_RoundTripBinary(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ch, err := pwws.Dial(ctx, pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// Include NUL and high bytes — proves binary-safety end to end.
	want := []byte{0, 1, 2, 0xff, 'a', 'b', 0, 'c'}
	if _, err := ch.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(ch, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("roundtrip mismatch: got %v want %v", got, want)
	}
}

func TestChannel_LargePayloadSpansMultipleReads(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()

	ctx := context.Background()
	ch, err := pwws.Dial(ctx, pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	want := make([]byte, 64*1024)
	for i := range want {
		want[i] = byte(i & 0xff)
	}
	if _, err := ch.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(ch, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("64 KiB roundtrip mismatch")
	}
}

func TestChannel_EncodeDecodeHooks(t *testing.T) {
	t.Parallel()
	// Server expects ttyd-style: client prepends '0' for stdin, server
	// prepends '0' for stdout. Decode strips the byte; Encode adds it.
	upgrader := gws.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt != gws.TextMessage || len(msg) < 1 || msg[0] != '0' {
				_ = c.WriteMessage(gws.TextMessage, []byte("E:bad-frame"))
				return
			}
			_ = c.WriteMessage(gws.TextMessage, append([]byte{'0'}, msg[1:]...))
		}
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ch, err := pwws.Dial(context.Background(), pwws.Options{
		URL: url,
		Encode: func(b []byte) ([]byte, int, error) {
			return append([]byte{'0'}, b...), pwws.TextMessage, nil
		},
		Decode: func(mt int, b []byte) ([]byte, error) {
			if mt != pwws.TextMessage || len(b) < 1 {
				return nil, errors.New("unexpected frame")
			}
			return b[1:], nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if _, err := ch.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(ch, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q want %q", got, "hello")
	}
}

func TestChannel_Resize(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		seenType int
		seenBody []byte
		gotFrame = make(chan struct{}, 1)
	)
	url, srv := startEcho(t, func(mt int, msg []byte) {
		mu.Lock()
		seenType, seenBody = mt, append([]byte(nil), msg...)
		mu.Unlock()
		select {
		case gotFrame <- struct{}{}:
		default:
		}
	})
	defer srv.Close()

	ch, err := pwws.Dial(context.Background(), pwws.Options{
		URL: url,
		EncodeResize: func(cols, rows uint16) ([]byte, int, error) {
			return []byte{byte(cols >> 8), byte(cols), byte(rows >> 8), byte(rows)},
				pwws.BinaryMessage, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if err := ch.Resize(context.Background(), 80, 24); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	select {
	case <-gotFrame:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw resize frame")
	}
	mu.Lock()
	defer mu.Unlock()
	if seenType != gws.BinaryMessage {
		t.Errorf("resize messageType = %d want %d", seenType, gws.BinaryMessage)
	}
	if len(seenBody) != 4 || seenBody[1] != 80 || seenBody[3] != 24 {
		t.Errorf("resize body = %v, want [_ 80 _ 24]", seenBody)
	}
}

func TestChannel_ResizeNoEncoderIsNoOp(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()
	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if err := ch.Resize(context.Background(), 80, 24); err != nil {
		t.Errorf("Resize without encoder should be no-op, got %v", err)
	}
}

func TestChannel_RemoteCloseGivesEOF(t *testing.T) {
	t.Parallel()
	upgrader := gws.Upgrader{}
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Send one message, then close politely.
		_ = c.WriteMessage(gws.BinaryMessage, []byte("bye"))
		_ = c.WriteControl(gws.CloseMessage,
			gws.FormatCloseMessage(gws.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = c.Close()
		close(closed)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	got := make([]byte, 3)
	if _, err := io.ReadFull(ch, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "bye" {
		t.Errorf("got %q want %q", got, "bye")
	}

	<-closed
	// Next read must surface EOF, not a generic websocket error.
	_, err = ch.Read(make([]byte, 16))
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestChannel_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()

	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestChannel_DialRetries_NoopOnHealthy(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()

	// Retries configured but the first attempt succeeds — retry path
	// must not add latency or extra connections on the happy case.
	ch, err := pwws.Dial(context.Background(), pwws.Options{
		URL:         url,
		DialRetries: 3,
		DialBackoff: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Dial with retries on healthy server: %v", err)
	}
	defer ch.Close()
}

func TestChannel_DialRetries_AllFail(t *testing.T) {
	t.Parallel()
	// Pick a port that is almost certainly closed (well above the
	// dynamic range we'd be unlucky to hit). 3 retries × 50ms = 150ms
	// of backoff total — well under the test budget.
	start := time.Now()
	_, err := pwws.Dial(context.Background(), pwws.Options{
		URL:         "ws://127.0.0.1:1",
		DialTimeout: 200 * time.Millisecond,
		DialRetries: 2,
		DialBackoff: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected dial failure after exhausting retries")
	}
	elapsed := time.Since(start)
	// At minimum we should see (50ms + 100ms) = 150ms of backoff
	// across 2 retries, plus the dial attempts themselves.
	if elapsed < 100*time.Millisecond {
		t.Errorf("retries appear skipped: elapsed=%v want >=100ms", elapsed)
	}
}

func TestChannel_DialRetries_BadHandshakeNoRetry(t *testing.T) {
	t.Parallel()
	// Server that completes TCP + HTTP but refuses the WS upgrade
	// with a 403 — gorilla returns ErrBadHandshake, which we treat
	// as terminal.
	calls := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	_, err := pwws.Dial(context.Background(), pwws.Options{
		URL:         url,
		DialRetries: 5, // would amplify the failure if we retried
		DialBackoff: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected ErrBadHandshake to fail dial")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("server saw %d calls; want exactly 1 (no retry on bad handshake)", calls)
	}
}

func TestChannel_DialTimeout(t *testing.T) {
	t.Parallel()
	// Listener that accepts TCP but never replies — DialContext should
	// honor the handshake timeout.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// hijack and hold the socket so the WS upgrade never completes
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("ResponseWriter not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	start := time.Now()
	_, err := pwws.Dial(context.Background(), pwws.Options{URL: url, DialTimeout: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("expected dial timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("DialTimeout not honored, took %v", elapsed)
	}
}

// TestChannel_RaceCleanReadAfterClose makes sure a Read pending when Close
// fires returns promptly rather than hanging.
func TestChannel_RaceCleanReadAfterClose(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()

	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}

	var unblocked atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ch.Read(make([]byte, 16))
		unblocked.Store(true)
	}()

	time.Sleep(50 * time.Millisecond)
	_ = ch.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close")
	}
	if !unblocked.Load() {
		t.Error("Read returned but flag not set")
	}
}

func TestChannel_CapsBinarySafe(t *testing.T) {
	t.Parallel()
	url, srv := startEcho(t, nil)
	defer srv.Close()
	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	c := ch.Caps()
	if !c.BinarySafe {
		t.Error("WebSocket Channel must report BinarySafe=true")
	}
	if c.MaxWriteChunk != 0 {
		t.Errorf("MaxWriteChunk = %d, want 0 (no limit)", c.MaxWriteChunk)
	}
}
