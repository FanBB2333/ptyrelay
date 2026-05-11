package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	gws "github.com/gorilla/websocket"
)

// startBashWS mirrors the bridge used by the WS Channel integration
// tests and the CLI tests: one bash-over-PTY subprocess per connection,
// raw bytes in BinaryMessage frames either way.
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

func buildMCP(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "ptyrelay-mcp")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/ptyrelay-mcp")
	cmd.Dir = repoRoot(t)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ptyrelay-mcp: %v\n%s", err, buildOut)
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

// mcpClient is a tiny stdio-JSON-RPC driver: it launches the MCP server
// binary, writes one request per line, and reads one response per line.
type mcpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	cancel context.CancelFunc
}

func startMCP(t *testing.T, bin string, env []string) *mcpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr // surface server logs in test output
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	return &mcpClient{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64*1024),
		cancel: cancel,
	}
}

func (c *mcpClient) Close() {
	_ = c.stdin.Close()
	_ = c.cmd.Wait()
	c.cancel()
}

func (c *mcpClient) call(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	body, _ := json.Marshal(req)
	if _, err := c.stdin.Write(append(body, '\n')); err != nil {
		t.Fatalf("write %s: %v", method, err)
	}
	line, err := c.stdout.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response for %s: %v", method, err)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode %s response: %v\nraw=%s", method, err, line)
	}
	return resp
}

func TestMCP_HandshakeAndToolsList(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=" + url,
		"PTYRELAY_NO_AGENT=1",
		"PTYRELAY_TIMEOUT=30s",
	})
	defer c.Close()

	init := c.call(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if init["error"] != nil {
		t.Fatalf("initialize error: %v", init["error"])
	}
	result, _ := init["result"].(map[string]any)
	if result == nil || result["protocolVersion"] == nil {
		t.Fatalf("initialize result missing protocolVersion: %v", init)
	}

	list := c.call(t, 2, "tools/list", nil)
	res, _ := list["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) < 5 {
		t.Fatalf("expected >=5 tools, got %d: %v", len(tools), tools)
	}
	want := map[string]bool{
		"read_file": false, "write_file": false, "run_command": false,
		"list_dir": false, "stat": false,
	}
	for _, t := range tools {
		m, _ := t.(map[string]any)
		want[m["name"].(string)] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestMCP_RunCommandTool(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=" + url,
		"PTYRELAY_NO_AGENT=1",
		"PTYRELAY_TIMEOUT=30s",
	})
	defer c.Close()

	_ = c.call(t, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
	})
	resp := c.call(t, 2, "tools/call", map[string]any{
		"name":      "run_command",
		"arguments": map[string]any{"command": "echo mcp-tool-marker"},
	})
	if resp["error"] != nil {
		t.Fatalf("tool error: %v", resp["error"])
	}
	res, _ := resp["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("empty content: %v", res)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "mcp-tool-marker") {
		t.Errorf("marker missing from output: %q", text)
	}
	if !strings.Contains(text, "exit_code: 0") {
		t.Errorf("exit_code line missing: %q", text)
	}
}

func TestMCP_WriteThenReadTool(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=" + url,
		"PTYRELAY_NO_AGENT=1",
		"PTYRELAY_TIMEOUT=30s",
	})
	defer c.Close()

	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	path := fmt.Sprintf("/tmp/ptyrelay-mcp-%d", os.Getpid())
	body := "hello mcp roundtrip"

	// write_file
	w := c.call(t, 2, "tools/call", map[string]any{
		"name": "write_file",
		"arguments": map[string]any{
			"path": path, "contents": body, "mode": 0o644,
		},
	})
	if w["error"] != nil {
		t.Fatalf("write_file error: %v", w["error"])
	}
	defer func() {
		_ = c.call(t, 99, "tools/call", map[string]any{
			"name": "run_command",
			"arguments": map[string]any{
				"command": "rm -f " + path,
			},
		})
	}()

	// read_file
	r := c.call(t, 3, "tools/call", map[string]any{
		"name":      "read_file",
		"arguments": map[string]any{"path": path},
	})
	if r["error"] != nil {
		t.Fatalf("read_file error: %v", r["error"])
	}
	res, _ := r["result"].(map[string]any)
	content, _ := res["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if text != body {
		t.Errorf("roundtrip mismatch: got %q want %q", text, body)
	}
}

// TestMCP_MkdirRenameRemove drives the three new filesystem tools
// against the bash-over-WS bridge. Tests share state intentionally:
// mkdir → rename → remove walks the directory through its lifecycle,
// matching the pattern an LLM operator would actually take.
func TestMCP_MkdirRenameRemove(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=" + url,
		"PTYRELAY_NO_AGENT=1",
		"PTYRELAY_TIMEOUT=30s",
	})
	defer c.Close()

	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	dir := fmt.Sprintf("/tmp/ptyrelay-mcp-mrr-%d", os.Getpid())
	moved := dir + "-moved"
	file := moved + "/payload"
	fileRenamed := moved + "/payload-renamed"
	// Best-effort cleanup if the test bails early.
	defer func() {
		_ = c.call(t, 99, "tools/call", map[string]any{
			"name":      "run_command",
			"arguments": map[string]any{"command": "rm -rf " + dir + " " + moved},
		})
	}()

	// mkdir + rename exercised on the directory.
	mk := c.call(t, 2, "tools/call", map[string]any{
		"name":      "mkdir",
		"arguments": map[string]any{"path": dir},
	})
	if mk["error"] != nil {
		t.Fatalf("mkdir error: %v", mk["error"])
	}
	mv := c.call(t, 3, "tools/call", map[string]any{
		"name":      "rename",
		"arguments": map[string]any{"old_path": dir, "new_path": moved},
	})
	if mv["error"] != nil {
		t.Fatalf("rename error: %v", mv["error"])
	}

	// Drop a file inside the renamed dir so we can hit remove + rename
	// on a regular file (shell.Backend.Remove is rm -f, file-only).
	w := c.call(t, 4, "tools/call", map[string]any{
		"name": "write_file",
		"arguments": map[string]any{
			"path": file, "contents": "x",
		},
	})
	if w["error"] != nil {
		t.Fatalf("write_file (setup): %v", w["error"])
	}
	mv2 := c.call(t, 5, "tools/call", map[string]any{
		"name":      "rename",
		"arguments": map[string]any{"old_path": file, "new_path": fileRenamed},
	})
	if mv2["error"] != nil {
		t.Fatalf("rename(file): %v", mv2["error"])
	}
	rm := c.call(t, 6, "tools/call", map[string]any{
		"name":      "remove",
		"arguments": map[string]any{"path": fileRenamed},
	})
	if rm["error"] != nil {
		t.Fatalf("remove(file): %v", rm["error"])
	}

	// Verify the file is gone via stat — its absence is the success
	// signal we're really proving roundtripped through the backend.
	st := c.call(t, 7, "tools/call", map[string]any{
		"name":      "stat",
		"arguments": map[string]any{"path": fileRenamed},
	})
	res, _ := st["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("stat returned empty content")
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "stat ") || !strings.Contains(strings.ToLower(text), "no such") {
		t.Errorf("post-remove stat unexpectedly succeeded: %q", text)
	}
}

func TestMCP_AgentInfoTool(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	url, srv := startBashWS(t)
	defer srv.Close()

	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=" + url,
		"PTYRELAY_NO_AGENT=1",
		"PTYRELAY_TIMEOUT=30s",
	})
	defer c.Close()

	_ = c.call(t, 1, "initialize", map[string]any{"protocolVersion": "2025-06-18"})

	resp := c.call(t, 2, "tools/call", map[string]any{
		"name":      "agent_info",
		"arguments": map[string]any{},
	})
	if resp["error"] != nil {
		t.Fatalf("agent_info error: %v", resp["error"])
	}
	res, _ := resp["result"].(map[string]any)
	content, _ := res["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	// --no-agent → shell-only backend
	if !strings.Contains(text, `"backend"`) || !strings.Contains(text, "shell") {
		t.Errorf("agent_info missing backend=shell: %q", text)
	}
	if !strings.Contains(text, `"transport"`) || !strings.Contains(text, "ws") {
		t.Errorf("agent_info missing transport=ws: %q", text)
	}
}

func TestMCP_UnknownMethodErrors(t *testing.T) {
	t.Parallel()
	bin := buildMCP(t)
	// No real transport — we never expect to dial since the method
	// itself is rejected before any backend interaction.
	c := startMCP(t, bin, []string{
		"PTYRELAY_TRANSPORT=ws",
		"PTYRELAY_WS_URL=ws://127.0.0.1:1", // unreachable; never dialed
		"PTYRELAY_NO_AGENT=1",
	})
	defer c.Close()

	resp := c.call(t, 1, "nonsense/method", nil)
	if resp["error"] == nil {
		t.Fatal("expected error for unknown method")
	}
	emap := resp["error"].(map[string]any)
	if emap["code"].(float64) != -32601 {
		t.Errorf("error code = %v, want -32601", emap["code"])
	}
}
