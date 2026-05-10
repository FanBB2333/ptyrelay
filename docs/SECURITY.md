# ptyrelay Security model

This document describes ptyrelay's threat model, what the project does
to mitigate the threats it considers in scope, and what's explicitly
**not** in scope. If you're considering ptyrelay for a sensitive
deployment, read it carefully — the boundary it draws is real.

## In scope

ptyrelay assumes the operator has *already* authenticated to the
remote shell through whatever mechanism owns that channel — SSH keys,
tmux session credentials, code-server auth, etc. ptyrelay rides on
top of that authenticated channel; it does not add or replace any
authentication of its own.

The threats we care about within that perimeter:

1. **Shell injection.** A user-supplied path or command must never be
   able to execute arbitrary additional commands on the remote.
2. **PTY echo confusion.** A nonce-substitution sequence must not let
   PTY-echoed bytes ever match the framing markers the parser uses.
3. **Agent integrity.** The agent binary AgentBackend talks to must be
   verified at install time so a rogue binary swap is detected.
4. **Side-channel leak via logs.** Sensitive bytes (payload data,
   secrets) must not land in human-readable logs at default verbosity.

## Out of scope

- **Channel confidentiality.** ptyrelay does not encrypt — the
  underlying transport (SSH, WSS) is responsible for that. If your
  channel is plaintext, your bytes are plaintext.
- **Authn/z.** ptyrelay inherits whatever access the channel grants.
  It does not implement RBAC.
- **Multi-tenant isolation.** A single Session/Backend talks to one
  remote shell. Concurrent unrelated sessions are the operator's
  problem.
- **Defending against a compromised remote.** If an attacker controls
  the remote shell, they own the data.
- **Defending against a compromised local CLI.** If the local
  ptyrelay binary is hostile, all bets are off.

## Mitigations

### Shell injection

Every path or argument that flows from the caller into a shell
command goes through `internal/shellquote.Quote`, which produces POSIX
single-quoted form (`'…'` with `'` → `'\''`). This is the only POSIX
quoting form that suppresses every metacharacter; double quotes still
expand `$`, backticks, and `\`.

`shellquote_test.go` round-trips a battery of pathological inputs —
embedded quotes, newlines, dollar signs, backslashes, command
substitution patterns — through `/bin/sh -c "printf '%s' <quoted>"` and
asserts the bytes that come back match the bytes that went in. If a
contributor breaks the escape, that test fails.

Caveat: paths beginning with `-` can still be misinterpreted as flag
arguments by some commands (e.g. `chmod -ABC file`). v0.2.0 omits the
`--` end-of-options separator from chmod/mkdir/mv/rm because BSD/macOS
versions don't accept it. Callers passing relative paths starting with
`-` should normalize them (e.g. prepend `./`) themselves; the
ShellBackend does not.

### PTY echo confusion

The Session-layer framing wrapper inserts the per-call nonce via a
**shell variable** rather than as a literal:

```sh
__PR_N=<nonce>; printf '\n__PR_BEG_'$__PR_N'__\n'; { <cmd>
}; __PR_RC=$?; printf '\n__PR_END_'$__PR_N'__:%d\n' "$__PR_RC"
```

Bytes the PTY echoes back to the master fd contain the literal
`$__PR_N` because echo doesn't expand variables. The bytes the runtime
`printf` produces contain the *substituted* nonce. The parser scans
for the substituted form, so PTY echo can never collide with the real
markers. This is the most security-sensitive design decision in the
codebase and is restated in `docs/ARCHITECTURE.md`.

The nonce is 16 hex chars (8 bytes from `crypto/rand`) — collision
probability is `2^-64` per call.

### Agent integrity

`Bootstrap` writes the agent through `ShellBackend.Write`, which:
1. Streams base64 chunks into a tempfile under the install directory.
2. Computes `sha256sum` (or `shasum -a 256`) on the remote tempfile.
3. Compares against the locally-computed digest of the binary bytes.
4. Aborts and removes the tempfile on mismatch (`backend.ErrCorrupted`).
5. Atomically renames into place only after the digest matches.

`VerifyInstall` then runs `ping` on the freshly-installed agent —
catches the case where the remote architecture mismatches the binary
(it would land but fail to exec).

A future v0.3.0 may sign the agent binary with a public key the local
CLI verifies before considering an existing on-remote agent
trustworthy. v0.2.0 trusts the bootstrap-time write.

### Side-channel leak via logs

Default-verbosity `slog` records do not include payload bytes. Debug
logging (`--debug`) caps any payload preview at 256 bytes and explicitly
excludes file contents and base64 envelopes. The `Result.Stdout` and
`Result.Stderr` fields produced by Run are NOT logged at any verbosity
— callers who want to log them must do it themselves at the call site.

Tempfile names (`*.tmp.<nonce>`) include random nonces but no payload
information, so `ls /tmp` doesn't surface ptyrelay activity meaningfully.

## Reporting issues

If you find a vulnerability, please open a GitHub Security Advisory
rather than a public issue. We aim to acknowledge within 72 hours and
ship a fix or detailed mitigation within 30 days.
