// Package proto defines the wire types and the framing codec for talking
// to ptyrelay-agent.
//
// The protocol is JSON for the message body, with two framing options:
//
//   - Newline-delimited JSON (used by [WriteOneShot]/[ReadOneShot])
//     for one-shot agent invocations driven by ShellBackend's
//     here-doc-based RunFramed. The body cannot contain raw newlines —
//     `json.Encoder` already escapes them.
//
//   - Length-prefixed JSON (used by [WriteFrame]/[ReadFrame]) for REPL
//     agents driven by Session.Pipe. Each message is `4-byte big-endian
//     length || JSON body`; allows binary-safe channels and clean
//     message boundaries.
//
// Binary payloads (file contents, stdin, stdout, stderr) inside JSON are
// base64-encoded by Go's `encoding/json` automatically — `[]byte` fields
// marshal to base64 strings.
package proto

import (
	"encoding/json"
	"errors"
	"io/fs"
	"time"
)

// Version is the protocol version. Bump on incompatible changes; clients
// MUST refuse to talk to a wire that doesn't match.
const Version = 1

// AgentVersion is the embedded agent build version. Bumped manually on
// agent releases; surfaced via the ping op.
const AgentVersion = "0.3.0"

// Op identifies a remote operation by name.
type Op string

const (
	OpPing     Op = "ping"
	OpRead     Op = "read"
	OpWrite    Op = "write"
	OpStat     Op = "stat"
	OpLstat    Op = "lstat"
	OpList     Op = "list"
	OpRemove   Op = "remove"
	OpRename   Op = "rename"
	OpMkdirAll Op = "mkdir_all"
	OpRun      Op = "run"

	// OpBye terminates a REPL agent loop. The agent replies with
	// `ok=true` and exits with code 0.
	OpBye Op = "bye"
)

// Request is the on-wire request envelope.
type Request struct {
	V    int             `json:"v"`
	ID   string          `json:"id,omitempty"`
	Op   Op              `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response is the on-wire response envelope.
//
// On success, OK is true and Data carries the op-specific payload.
// On error, OK is false, Err is a human-readable message, and ErrKind
// is a machine-readable category that the caller maps to typed errors.
type Response struct {
	V       int             `json:"v"`
	ID      string          `json:"id,omitempty"`
	OK      bool            `json:"ok"`
	Data    json.RawMessage `json:"data,omitempty"`
	Err     string          `json:"err,omitempty"`
	ErrKind string          `json:"errKind,omitempty"`
}

// ErrKind values used in Response.ErrKind. The agent maps Go errors to
// these categories so the client doesn't have to parse Err strings.
const (
	ErrKindUnknownOp     = "unknown_op"
	ErrKindBadArgs       = "bad_args"
	ErrKindNotFound      = "not_found"
	ErrKindPermission    = "permission"
	ErrKindIO            = "io"
	ErrKindBadProto      = "bad_protocol"
	ErrKindUnimplemented = "unimplemented"
	ErrKindInternal      = "internal"
)

// ----- Op-specific arg / data types -----

// PingData is returned by the ping op.
type PingData struct {
	Version      int      `json:"version"`
	AgentVersion string   `json:"agentVersion"`
	Caps         []string `json:"caps"`
}

// ReadArgs / ReadData — read one file's contents.
type ReadArgs struct {
	Path string `json:"path"`
}
type ReadData struct {
	Bytes []byte `json:"bytes"`
}

// WriteArgs — replace one file's contents.
type WriteArgs struct {
	Path  string      `json:"path"`
	Bytes []byte      `json:"bytes"`
	Mode  fs.FileMode `json:"mode"`
}

// StatArgs / StatData — stat (or lstat) one path.
type StatArgs struct {
	Path string `json:"path"`
}
type StatData struct {
	Name          string    `json:"name"`
	Size          int64     `json:"size"`
	Mode          uint32    `json:"mode"`
	ModTime       time.Time `json:"modTime"`
	IsDir         bool      `json:"isDir"`
	IsSymlink     bool      `json:"isSymlink"`
	SymlinkTarget string    `json:"symlinkTarget,omitempty"`
}

// ListArgs / ListData — list a directory's immediate children.
type ListArgs struct {
	Path string `json:"path"`
}
type ListData struct {
	Entries []StatData `json:"entries"`
}

// RemoveArgs — remove a single file or empty directory.
type RemoveArgs struct {
	Path string `json:"path"`
}

// RenameArgs — rename old → new.
type RenameArgs struct {
	OldPath string `json:"oldPath"`
	NewPath string `json:"newPath"`
}

// MkdirAllArgs — mkdir -p with the given mode on the leaf.
type MkdirAllArgs struct {
	Path string      `json:"path"`
	Mode fs.FileMode `json:"mode"`
}

// RunArgs / RunData — execute a shell command.
type RunArgs struct {
	Cmd   string `json:"cmd"`
	Stdin []byte `json:"stdin,omitempty"`
}
type RunData struct {
	Stdout   []byte        `json:"stdout,omitempty"`
	Stderr   []byte        `json:"stderr,omitempty"`
	ExitCode int           `json:"exitCode"`
	Duration time.Duration `json:"duration"`
}

// ErrUnknownOp is returned by handlers that don't recognize the op.
var ErrUnknownOp = errors.New("proto: unknown op")
