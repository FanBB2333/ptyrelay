package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/creack/pty"
	gws "github.com/gorilla/websocket"
)

// startBashWS stands up an httptest WS endpoint that bridges to a fresh
// `bash --noprofile --norc -i` per connection. Used to exercise the CLI
// end-to-end against a real shell without needing tmux.
func startBashWS(t *testing.T) (string, *httptest.Server) {
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
		cmd := exec.Command("bash", "--noprofile", "--norc", "-i")
		ptmx, err := pty.Start(cmd)
		if err != nil {
			_ = ws.Close()
			return
		}
		var once sync.Once
		shutdown := func() {
			once.Do(func() {
				_ = ptmx.Close()
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
				_ = cmd.Wait()
				_ = ws.Close()
			})
		}
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
			}
		}()
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

// runCLI invokes the just-built ptyrelay binary with args and returns
// its exit code + stdout + stderr.
func runCLI(t *testing.T, bin string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errorsAs(err, &ee) {
			t.Fatalf("CLI invocation failed: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

// errorsAs is a tiny inlined errors.As to keep this test file dep-light.
func errorsAs(err error, target **exec.ExitError) bool {
	for err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func buildCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "ptyrelay")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/ptyrelay")
	cmd.Dir = repoRoot(t)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ptyrelay: %v\n%s", err, buildOut)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	for d := cwd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("go.mod not found")
	return ""
}

func TestCLI_Exec(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	code, stdout, stderr := runCLI(t, bin,
		"exec", "--ws", url, "--no-agent", "--timeout", "30s",
		"--", "echo", "hello-from-cli")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "hello-from-cli") {
		t.Errorf("missing marker: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestCLI_PutAndGetRoundTrip(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	scratch := t.TempDir()
	local := filepath.Join(scratch, "payload")
	body := []byte("hello ptyrelay cli\n")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}
	remote := "/tmp/ptyrelay-cli-" + filepath.Base(scratch)

	// Put
	code, _, stderr := runCLI(t, bin,
		"put", "--ws", url, "--no-agent", "--timeout", "30s",
		local, remote)
	if code != 0 {
		t.Fatalf("put exit=%d stderr=%q", code, stderr)
	}
	defer runCLI(t, bin, "exec", "--ws", url, "--no-agent", "--", "rm", "-f", remote)

	// Get to stdout
	code, stdout, stderr := runCLI(t, bin,
		"get", "--ws", url, "--no-agent", "--timeout", "30s",
		remote)
	if code != 0 {
		t.Fatalf("get exit=%d stderr=%q", code, stderr)
	}
	if stdout != string(body) {
		t.Errorf("roundtrip mismatch:\ngot:  %q\nwant: %q", stdout, body)
	}
}

func TestCLI_ExecTransport(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}
	bin := buildCLI(t)

	// --exec spawns bash locally and uses its stdio as the channel.
	// Same Session+ShellBackend stack, no remote host required.
	code, stdout, stderr := runCLI(t, bin,
		"exec",
		"--exec", "bash --noprofile --norc",
		"--no-agent", "--timeout", "30s",
		"--", "echo", "exec-transport-marker")
	if code != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, "exec-transport-marker") {
		t.Errorf("marker missing: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestCLI_TransportMutuallyExclusive(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)

	// Passing both --ws and --exec must be rejected with a clear
	// error rather than silently picking one.
	code, _, stderr := runCLI(t, bin,
		"exec",
		"--ws", "ws://127.0.0.1:1",
		"--exec", "bash",
		"--", "true")
	if code == 0 {
		t.Errorf("expected non-zero exit when both transports set, got 0")
	}
	if !strings.Contains(stderr, "exactly one of") {
		t.Errorf("stderr missing exclusivity message: %q", stderr)
	}
}

func TestCLI_HelpAndUsage(t *testing.T) {
	t.Parallel()
	bin := buildCLI(t)

	// `help` is exit 0 with usage on stdout.
	code, stdout, _ := runCLI(t, bin, "help")
	if code != 0 {
		t.Errorf("help exit=%d", code)
	}
	if !strings.Contains(stdout, "Subcommands:") {
		t.Errorf("help output missing 'Subcommands:': %q", stdout)
	}

	// No args is exit 2 with usage on stderr.
	code, _, stderr := runCLI(t, bin)
	if code != 2 {
		t.Errorf("no-args exit=%d want 2", code)
	}
	if !strings.Contains(stderr, "Subcommands:") {
		t.Errorf("no-args stderr missing 'Subcommands:': %q", stderr)
	}

	// Bogus subcommand is exit 2.
	code, _, stderr = runCLI(t, bin, "bogus")
	if code != 2 {
		t.Errorf("bogus exit=%d want 2", code)
	}
	if !strings.Contains(stderr, "unknown subcommand") {
		t.Errorf("bogus stderr: %q", stderr)
	}
}
