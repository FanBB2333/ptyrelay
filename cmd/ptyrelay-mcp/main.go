// Command ptyrelay-mcp is a Model Context Protocol (MCP) server that
// exposes ptyrelay's RemoteFS / RemoteExec surface as MCP tools.
//
// Transport: stdio JSON-RPC 2.0. Each line on stdin is one request;
// each line on stdout is one response or notification. Stderr is free
// for logs and is not part of the protocol.
//
// The remote target is configured via environment variables, so the MCP
// client (typically Claude Code) only needs to launch the binary:
//
//	PTYRELAY_TRANSPORT  tmux | ws        (required)
//	PTYRELAY_TMUX_PANE  pane target      (if transport=tmux)
//	PTYRELAY_TMUX_SOCK  socket path      (optional)
//	PTYRELAY_WS_URL     ws://… or wss:// (if transport=ws)
//	PTYRELAY_SHELL      bash|zsh|dash|sh (default bash)
//	PTYRELAY_AGENT      remote agent path (default ~/.local/bin/ptyrelay-agent)
//	PTYRELAY_NO_AGENT   1 to force ShellBackend only
//	PTYRELAY_TIMEOUT    per-call timeout (default 60s, Go duration format)
//
// MCP method coverage:
//
//	initialize      handshake
//	notifications/initialized   (ack — no-op)
//	tools/list      enumerate exposed tools
//	tools/call      execute one tool
//	shutdown        graceful exit (we just close stdin)
//
// Tools:
//
//	read_file       {path} → {contents}
//	write_file      {path, contents, mode?} → {ok}
//	run_command     {command, stdin?} → {stdout, stderr, exit_code}
//	list_dir        {path} → {entries: [{name,size,mode,is_dir}]}
//	stat            {path, follow?} → {name,size,mode,modTime,isDir,isSymlink}
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
)

const protocolVersion = "2025-06-18"

func main() {
	log := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "ptyrelay-mcp: "+format+"\n", args...)
	}

	cfg, err := loadConfig()
	if err != nil {
		log("config: %v", err)
		os.Exit(2)
	}

	srv := &server{cfg: cfg, log: log}
	if err := srv.run(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		log("server: %v", err)
		os.Exit(1)
	}
}

// server runs the JSON-RPC loop and dispatches MCP methods.
//
// A single Backend connection is shared across all tools/call requests
// for the lifetime of the process. Each call gets its own ctx with the
// configured timeout.
type server struct {
	cfg *config
	log func(string, ...any)

	be    backend.Backend // lazily dialed on first tools/call
	close func()
}

func (s *server) run(in io.Reader, out io.Writer) error {
	defer func() {
		if s.close != nil {
			s.close()
		}
	}()
	dec := bufio.NewScanner(in)
	// Some MCP clients send large `tools/call` payloads. Bump the line
	// buffer to a generous ceiling.
	dec.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(out)
	for dec.Scan() {
		line := dec.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log("malformed JSON: %v", err)
			continue
		}
		resp, isNotification := s.handle(&req)
		if isNotification {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return dec.Err()
}

// handle dispatches one request to the right method handler. Returns
// (response, true) for a notification (no reply due) or (response, false)
// otherwise.
func (s *server) handle(req *rpcRequest) (*rpcResponse, bool) {
	// Notifications carry no ID and never expect a response.
	if req.ID == nil {
		// Recognize the standard initialized notification — silently
		// accept anything else.
		return nil, true
	}
	switch req.Method {
	case "initialize":
		return reply(req.ID, initializeResult{
			ProtocolVersion: protocolVersion,
			ServerInfo:      serverInfo{Name: "ptyrelay-mcp", Version: "0.3.0"},
			Capabilities:    serverCapabilities{Tools: &emptyObject{}},
		}), false
	case "tools/list":
		return reply(req.ID, toolsListResult{Tools: toolSpecs()}), false
	case "tools/call":
		var p toolsCallParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errReply(req.ID, -32602, "invalid params: "+err.Error()), false
		}
		result, err := s.callTool(p)
		if err != nil {
			return errReply(req.ID, -32603, err.Error()), false
		}
		return reply(req.ID, result), false
	case "shutdown":
		return reply(req.ID, emptyObject{}), false
	default:
		return errReply(req.ID, -32601, "method not found: "+req.Method), false
	}
}

// dial ensures s.be is connected, dialing on the first request.
func (s *server) dial() error {
	if s.be != nil {
		return nil
	}
	be, close, err := dialFromConfig(context.Background(), s.cfg)
	if err != nil {
		return err
	}
	s.be = be
	s.close = close
	return nil
}
