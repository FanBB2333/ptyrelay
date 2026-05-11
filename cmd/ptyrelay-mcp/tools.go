package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/FanBB2333/ptyrelay/pkg/backend/router"
)

// toolSpecs returns the static MCP tool descriptors served by tools/list.
// Schemas use the small JSON Schema subset MCP clients actually inspect.
func toolSpecs() []toolSpec {
	stringProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	return []toolSpec{
		{
			Name:        "read_file",
			Description: "Read a remote file and return its contents as UTF-8 text. Use only for text files; binary content will be lossy.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Absolute path on the remote host."),
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "write_file",
			Description: "Atomically write text contents to a remote file (tempfile + rename + sha256 verify).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":     stringProp("Absolute path on the remote host."),
					"contents": stringProp("File contents (UTF-8)."),
					"mode": map[string]any{
						"type":        "integer",
						"description": "Unix mode (octal int, e.g. 420 == 0o644). Defaults to 420.",
					},
				},
				"required": []string{"path", "contents"},
			},
		},
		{
			Name:        "run_command",
			Description: "Run a shell command on the remote host. Returns stdout, stderr (when the agent path is active), and exit code.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": stringProp("Command line, executed by the remote shell."),
					"stdin":   stringProp("Optional stdin to pipe in (UTF-8)."),
				},
				"required": []string{"command"},
			},
		},
		{
			Name:        "list_dir",
			Description: "List the immediate children of a remote directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Directory path on the remote host."),
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "stat",
			Description: "Return metadata for a remote path (size, mode, mtime, isDir, isSymlink).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Path on the remote host."),
					"follow": map[string]any{
						"type":        "boolean",
						"description": "Follow symlinks (default true); set false for lstat.",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "mkdir",
			Description: "Create a remote directory and any missing parents (mkdir -p). Idempotent.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Directory path to create."),
					"mode": map[string]any{
						"type":        "integer",
						"description": "Unix mode for newly created dirs (octal int, e.g. 493 == 0o755). Defaults to 493.",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "rename",
			Description: "Rename or move a remote path. Idempotent (same source+dest pair re-runs cleanly).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"old_path": stringProp("Existing absolute path on the remote host."),
					"new_path": stringProp("Destination absolute path."),
				},
				"required": []string{"old_path", "new_path"},
			},
		},
		{
			Name:        "remove",
			Description: "Remove a remote file or empty directory. NonIdempotent — a missing target is an error, not a no-op.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": stringProp("Path to remove (file or empty directory)."),
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "agent_info",
			Description: "Report which backend (agent or shell) is currently serving requests and basic identity (transport, agent path, healthy flag). Useful when an LLM needs to decide whether to expect separate stderr or honor MaxWriteChunk.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// callTool dispatches a tools/call to the right handler. Errors raised
// here become MCP "isError" content rather than JSON-RPC errors, so the
// LLM can recover gracefully.
func (s *server) callTool(p toolsCallParams) (toolsCallResult, error) {
	if err := s.dial(); err != nil {
		return errorContent("dial: " + err.Error()), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()

	switch p.Name {
	case "read_file":
		return s.toolReadFile(ctx, p.Arguments), nil
	case "write_file":
		return s.toolWriteFile(ctx, p.Arguments), nil
	case "run_command":
		return s.toolRunCommand(ctx, p.Arguments), nil
	case "list_dir":
		return s.toolListDir(ctx, p.Arguments), nil
	case "stat":
		return s.toolStat(ctx, p.Arguments), nil
	case "mkdir":
		return s.toolMkdir(ctx, p.Arguments), nil
	case "rename":
		return s.toolRename(ctx, p.Arguments), nil
	case "remove":
		return s.toolRemove(ctx, p.Arguments), nil
	case "agent_info":
		return s.toolAgentInfo(), nil
	default:
		return errorContent("unknown tool: " + p.Name), nil
	}
}

func (s *server) toolReadFile(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("read_file: missing required arg 'path'")
	}
	data, err := s.be.Read(ctx, a.Path)
	if err != nil {
		return errorContent(fmt.Sprintf("read %s: %v", a.Path, err))
	}
	return textContent(string(data))
}

func (s *server) toolWriteFile(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path     string `json:"path"`
		Contents string `json:"contents"`
		Mode     int    `json:"mode"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("write_file: missing 'path' or bad args")
	}
	mode := os.FileMode(a.Mode)
	if mode == 0 {
		mode = 0o644
	}
	if err := s.be.Write(ctx, a.Path, []byte(a.Contents), mode); err != nil {
		return errorContent(fmt.Sprintf("write %s: %v", a.Path, err))
	}
	return textContent(fmt.Sprintf("wrote %d bytes to %s", len(a.Contents), a.Path))
}

func (s *server) toolRunCommand(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Command string `json:"command"`
		Stdin   string `json:"stdin"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Command == "" {
		return errorContent("run_command: missing 'command'")
	}
	res, err := s.be.Run(ctx, a.Command, []byte(a.Stdin))
	if err != nil {
		return errorContent("run: " + err.Error())
	}
	// Render as a compact, LLM-friendly text blob — easier to read in
	// chat transcripts than a JSON dump.
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if len(res.Stdout) > 0 {
		fmt.Fprintf(&b, "stdout:\n%s\n", string(res.Stdout))
	}
	if len(res.Stderr) > 0 {
		fmt.Fprintf(&b, "stderr:\n%s\n", string(res.Stderr))
	}
	r := textContent(b.String())
	if res.ExitCode != 0 {
		r.IsError = true
	}
	return r
}

func (s *server) toolListDir(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("list_dir: missing 'path'")
	}
	entries, err := s.be.List(ctx, a.Path)
	if err != nil {
		return errorContent(fmt.Sprintf("list %s: %v", a.Path, err))
	}
	out, _ := json.MarshalIndent(entries, "", "  ")
	return textContent(string(out))
}

func (s *server) toolMkdir(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path string `json:"path"`
		Mode int    `json:"mode"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("mkdir: missing 'path' or bad args")
	}
	mode := os.FileMode(a.Mode)
	if mode == 0 {
		mode = 0o755
	}
	if err := s.be.MkdirAll(ctx, a.Path, mode); err != nil {
		return errorContent(fmt.Sprintf("mkdir %s: %v", a.Path, err))
	}
	return textContent(fmt.Sprintf("created %s (mode %o)", a.Path, mode))
}

func (s *server) toolRename(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.OldPath == "" || a.NewPath == "" {
		return errorContent("rename: missing 'old_path' or 'new_path'")
	}
	if err := s.be.Rename(ctx, a.OldPath, a.NewPath); err != nil {
		return errorContent(fmt.Sprintf("rename %s -> %s: %v", a.OldPath, a.NewPath, err))
	}
	return textContent(fmt.Sprintf("renamed %s -> %s", a.OldPath, a.NewPath))
}

func (s *server) toolRemove(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("remove: missing 'path'")
	}
	if err := s.be.Remove(ctx, a.Path); err != nil {
		return errorContent(fmt.Sprintf("remove %s: %v", a.Path, err))
	}
	return textContent("removed " + a.Path)
}

// toolAgentInfo answers the diagnostic "what am I talking to" question.
// We surface the routing-level view (which Backend is active, which
// transport configured it) so an LLM can reason about Caps before
// issuing expensive ops.
func (s *server) toolAgentInfo() toolsCallResult {
	info := map[string]any{
		"transport": s.cfg.Transport,
		"shell":     s.cfg.Shell.String(),
		"backend":   "shell", // default; overridden below if we have a router
	}
	if rb, ok := s.be.(*router.Backend); ok {
		if rb.AgentHealthy() {
			info["backend"] = "router(agent+shell)"
		} else {
			info["backend"] = "router(shell-only;agent_unhealthy)"
		}
		info["agent_path"] = rb.Agent().AgentPath()
	}
	out, _ := json.MarshalIndent(info, "", "  ")
	return textContent(string(out))
}

func (s *server) toolStat(ctx context.Context, raw json.RawMessage) toolsCallResult {
	var a struct {
		Path   string `json:"path"`
		Follow *bool  `json:"follow"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Path == "" {
		return errorContent("stat: missing 'path'")
	}
	follow := a.Follow == nil || *a.Follow
	var fi any
	var err error
	if follow {
		fi, err = s.be.Stat(ctx, a.Path)
	} else {
		fi, err = s.be.Lstat(ctx, a.Path)
	}
	if err != nil {
		return errorContent(fmt.Sprintf("stat %s: %v", a.Path, err))
	}
	out, _ := json.MarshalIndent(fi, "", "  ")
	return textContent(string(out))
}

