package main

import "encoding/json"

// JSON-RPC 2.0 types. We hand-roll these instead of pulling in a
// dependency — MCP only needs a couple of method shapes.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // raw to preserve int/string types
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func reply(id json.RawMessage, result any) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func errReply(id json.RawMessage, code int, msg string) *rpcResponse {
	return &rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// emptyObject lets us emit `{}` for "no payload" results without nil
// becoming `null`.
type emptyObject struct{}

// --- MCP-specific message shapes ----------------------------------------

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	ServerInfo      serverInfo         `json:"serverInfo"`
	Capabilities    serverCapabilities `json:"capabilities"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type serverCapabilities struct {
	// Tools is a capability marker: presence means we support tools/list
	// and tools/call. MCP currently doesn't define a payload, so an
	// empty object satisfies the spec.
	Tools *emptyObject `json:"tools,omitempty"`
}

type toolsListResult struct {
	Tools []toolSpec `json:"tools"`
}

// toolSpec is the public-facing description of a tool. inputSchema
// follows JSON Schema (Draft 2020-12 per MCP spec).
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// toolsCallResult is what MCP expects back from tools/call.
//
// Content is a discriminated array of content items; we only emit
// text items because everything ptyrelay returns is either text or
// base64-of-bytes (which we render as text).
type toolsCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type toolContent struct {
	Type string `json:"type"` // always "text" for us
	Text string `json:"text"`
}

func textContent(s string) toolsCallResult {
	return toolsCallResult{Content: []toolContent{{Type: "text", Text: s}}}
}

func errorContent(msg string) toolsCallResult {
	return toolsCallResult{
		Content: []toolContent{{Type: "text", Text: msg}},
		IsError: true,
	}
}
