package websocket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	pwws "github.com/FanBB2333/ptyrelay/pkg/channel/websocket"
	"github.com/FanBB2333/ptyrelay/pkg/session"
	"github.com/creack/pty"
	gws "github.com/gorilla/websocket"
)

// startBashOverWS stands up an httptest WebSocket server that bridges
// each connection to a fresh `bash --noprofile --norc -i` subprocess
// running behind a real PTY. WS BinaryMessage frames in either direction
// are stdin / stdout bytes verbatim — the simplest possible
// stdio-over-WS bridge, equivalent to a hand-rolled `socat ws bash`.
//
// This is exactly the shape v0.3.0 promises: a different transport
// implementation that the rest of the stack (Session / Backend) is
// unaware of.
func startBashOverWS(t *testing.T) (string, *httptest.Server) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}
	upgrader := gws.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// One bash + PTY per connection.
		cmd := exec.Command("bash", "--noprofile", "--norc", "-i")
		ptmx, err := pty.Start(cmd)
		if err != nil {
			_ = ws.Close()
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		var once sync.Once
		shutdown := func() {
			once.Do(func() {
				cancel()
				_ = ptmx.Close()
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				_ = ws.Close()
			})
		}
		// PTY → WS: pump pty bytes out as binary frames. A small write
		// buffer keeps frame count modest without losing latency.
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					if werr := ws.WriteMessage(gws.BinaryMessage, buf[:n]); werr != nil {
						shutdown()
						return
					}
				}
				if err != nil {
					shutdown()
					return
				}
				if ctx.Err() != nil {
					return
				}
			}
		}()
		// WS → PTY: every received frame is stdin bytes.
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				shutdown()
				return
			}
			if _, err := ptmx.Write(msg); err != nil {
				shutdown()
				return
			}
		}
	}))
	return "ws" + strings.TrimPrefix(srv.URL, "http"), srv
}

// TestE2E_SessionOverWebSocket is the v0.3.0 closure test: it runs the
// real ShellBackend over a WS-backed Channel and expects everything to
// just work, with no Backend-level changes.
func TestE2E_SessionOverWebSocket(t *testing.T) {
	t.Parallel()
	url, srv := startBashOverWS(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := pwws.Dial(ctx, pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	// Sanity probe: the backend can talk to the remote bash.
	if err := sb.Probe(ctx); err != nil {
		t.Fatalf("Probe over WS: %v", err)
	}

	// RemoteExec round-trip.
	res, err := sb.Run(ctx, "echo over-ws-stdout && echo over-ws-stderr 1>&2", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	combined := append(res.Stdout, res.Stderr...)
	if !strings.Contains(string(combined), "over-ws-stdout") {
		t.Errorf("missing stdout marker; got %q", combined)
	}

	// RemoteFS atomic write + read round-trip.
	tmp := "/tmp/ptyrelay-ws-e2e.txt"
	want := []byte("hello via websocket\n")
	if err := sb.Write(ctx, tmp, want, 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	defer func() { _ = sb.Remove(ctx, tmp) }()

	got, err := sb.Read(ctx, tmp)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read-back mismatch: got %q want %q", got, want)
	}
}

// TestE2E_RemoteHangupYieldsEOF verifies the read-side EOF mapping holds
// when the bash subprocess on the other end terminates voluntarily —
// the bridge closes the WS politely, and our Channel surfaces io.EOF.
func TestE2E_RemoteHangupYieldsEOF(t *testing.T) {
	t.Parallel()
	upgrader := gws.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = ws.WriteMessage(gws.BinaryMessage, []byte("hi\n"))
		_ = ws.WriteControl(gws.CloseMessage,
			gws.FormatCloseMessage(gws.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		_ = ws.Close()
	}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	ch, err := pwws.Dial(context.Background(), pwws.Options{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	got, err := io.ReadAll(ch)
	if !errors.Is(err, io.EOF) && err != nil {
		t.Errorf("ReadAll returned %v, expected nil or io.EOF", err)
	}
	if string(got) != "hi\n" {
		t.Errorf("got %q want %q", got, "hi\n")
	}
}
