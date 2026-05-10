# ptyrelay agent protocol

This document is the canonical wire format spec for the protocol spoken
between AgentBackend (in `pkg/backend/agent`) and the remote
`ptyrelay-agent` binary (in `cmd/ptyrelay-agent`). Implementations
deviate at their own peril.

## Versioning

The protocol carries a numeric version field on every message. v0.2.0
ships protocol version **1**. A peer that receives a request whose
`v` doesn't match its supported set MUST respond with an error of kind
`bad_protocol`.

## Transports

Two transports are defined. Both carry the same JSON message shape;
only the framing around each message differs.

### One-shot (here-doc / file)

Used by AgentBackend's v0.2.0 default path. The client invokes the
agent as `ptyrelay-agent --mode=one-shot`, delivering the request body
through stdin and reading the response from stdout. **Exactly one**
request/response pair is exchanged before the agent exits.

The body is a single JSON object terminated by a newline. The message
must NOT contain a literal newline outside JSON-escaped strings —
`json.Encoder` does this naturally; hand-rolled encoders must avoid
non-string newlines.

In practice AgentBackend stages the request bytes on the remote in
short `printf '%s' >> tmp` chunks (so each shell command stays under
the macOS PTY MAX_INPUT) and then runs the agent reading from that
file. The wire format the agent sees is unchanged.

### REPL (line-delimited)

Used by AgentBackend's v0.2.0 REPL path over `Session.Pipe`. The
client invokes `ptyrelay-agent --mode=repl` and exchanges multiple
request/response pairs without restarting the process.

Each message is the same shape as one-shot: a single JSON object
terminated by `\n`. We deliberately use line-delimited (rather than
length-prefixed) framing for the REPL transport because PTYs are not
binary-safe — a 4-byte big-endian length header could carry byte
values that the slave PTY's line discipline interprets (e.g. `\x03`
== Ctrl-C, `\x04` == EOT), and turning every relevant flag off in
stty is fragile across shells.

Length-prefixed framing (`proto.WriteFrame` / `ReadFrame`) is still
provided in `pkg/proto` and is the right choice for binary-safe
transports — a future WebSocket or kubectl-exec channel will adopt it.

The client signals "I'm done" with the `bye` op; the agent replies
with `ok=true` and exits cleanly. EOF on stdin also terminates the
agent.

Behavioral note for clients: the streaming consumer that reads the
agent's stdout (`io.PipeReader` returned by `Session.Pipe`) may pick
up one extra trailing `\n` at the very end of the byte stream, in
boundary cases where the wrapper's leading newline gets flushed
before the END marker becomes detectable. Line-delimited JSON
parsers tolerate this; strict-byte consumers should use `RunFramed`.

## Message shape

### Request

```json
{
  "v":   <integer>,    // protocol version, MUST be 1 in v0.2.0
  "id":  "<string>",   // optional, echoed back in the response
  "op":  "<string>",   // op name (see below)
  "args": <object>      // op-specific (see below)
}
```

Unknown fields MUST be ignored by both peers.

### Response

```json
{
  "v":       <integer>,
  "id":      "<string>",
  "ok":      <bool>,
  "data":    <object>,    // present iff ok=true; op-specific
  "err":     "<string>",  // present iff ok=false; human-readable
  "errKind": "<string>"   // present iff ok=false; machine-readable
}
```

`errKind` values are listed below — they are the contract a client
matches against, not `err`. New `errKind` values may be added; clients
MUST treat unrecognized values as the equivalent of `internal`.

## Ops

Every op has typed args/data. The columns are: name (wire), idempotency
class (used by RouterBackend), what `data` carries on success.

| Op           | Class           | Args                                    | Data                                                    |
|--------------|-----------------|-----------------------------------------|---------------------------------------------------------|
| `ping`       | ReadOnly        | (none)                                  | `{version,agentVersion,caps:[]string}`                  |
| `read`       | ReadOnly        | `{path}`                                | `{bytes}` — base64 of file contents                     |
| `write`      | Idempotent      | `{path,bytes,mode}`                     | (none) — agent writes atomically (tempfile + rename)    |
| `stat`       | ReadOnly        | `{path}`                                | `{name,size,mode,modTime,isDir,isSymlink,symlinkTarget}` |
| `lstat`      | ReadOnly        | `{path}`                                | same as stat (symlink not followed)                     |
| `list`       | ReadOnly        | `{path}`                                | `{entries: []StatData}` — sorted by name                |
| `remove`     | NonIdempotent   | `{path}`                                | (none)                                                  |
| `rename`     | Idempotent      | `{oldPath,newPath}`                     | (none)                                                  |
| `mkdir_all`  | Idempotent      | `{path,mode}`                           | (none)                                                  |
| `run`        | NonIdempotent   | `{cmd,stdin}`                           | `{stdout,stderr,exitCode,duration}`                     |
| `bye`        | (REPL-only)     | (none)                                  | (none) — agent exits with code 0                        |

### Field semantics

- `bytes` (read/write/run.stdin/run.stdout/run.stderr): `[]byte` in Go
  source, base64-encoded automatically by `encoding/json`.
- `mode`: full Unix mode bits as `uint32`. Permissions are `mode & 0o7777`.
  `os.FileMode` constants are stable across the wire.
- `modTime`: RFC 3339 time.
- `duration` (run): nanoseconds, JSON number.
- `stat` follows symlinks; `lstat` does not. `lstat` populates
  `symlinkTarget` for symlinks; `stat` never reports `isSymlink=true`.

## Error kinds

| `errKind`        | Meaning                                              | Wrapped Go error              |
|------------------|------------------------------------------------------|-------------------------------|
| `unknown_op`     | The agent doesn't recognize this op.                 | (none)                        |
| `bad_args`       | The args don't unmarshal into the op's schema.       | (none)                        |
| `not_found`      | The referenced path doesn't exist.                   | `os.ErrNotExist`              |
| `permission`     | The agent isn't allowed to access the path.         | `os.ErrPermission`            |
| `io`             | A generic IO failure on the remote.                  | (none — `err` carries detail) |
| `bad_protocol`   | Protocol version mismatch / malformed frame.        | `backend.ErrTransient`        |
| `unimplemented`  | The op is recognized but not yet supported.          | (none)                        |
| `internal`       | An unexpected agent-side failure.                    | (none)                        |

The Go client `pkg/backend/agent` wraps these via `fmt.Errorf("%w", …)`
so callers can do `errors.Is(err, os.ErrNotExist)` regardless of which
backend produced the error.

## Compatibility expectations

Within a major protocol version (v1.x), the agent MUST:
- Accept new optional fields on the request without complaining.
- Respond to a `ping` with at minimum `{version,agentVersion,caps}`.
- Treat unknown ops as `unknown_op` errors (not crashes).

Bumping the major version is reserved for incompatible changes —
removing an op, changing the framing, redefining a field type. Any
such change requires bumping `proto.Version` and shipping a new agent
binary.
