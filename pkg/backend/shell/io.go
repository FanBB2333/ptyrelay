package shell

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
	"github.com/FanBB2333/ptyrelay/pkg/backend"
)

// Read implements [backend.RemoteFS]. Files larger than MaxShellFileSize
// return ErrTooLarge — callers should use OpenRead for those.
func (b *Backend) Read(ctx context.Context, path string) ([]byte, error) {
	p, err := b.ensureProbed(ctx)
	if err != nil {
		return nil, err
	}

	// Cheap size check up front so we don't haul a 4 GiB file through
	// a 256-byte command line.
	info, err := b.Stat(ctx, path)
	if err == nil && info.Size > int64(b.maxShellFileSize) {
		return nil, fmt.Errorf("%w: %d bytes (limit %d)",
			backend.ErrTooLarge, info.Size, b.maxShellFileSize)
	}

	cmd := fmt.Sprintf("cat -- %s | %s", shellquote.Quote(path), p.b64EncodeCmd())
	res, err := b.runShell(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, shellError(backend.OpRead, res.ExitCode, res.Output)
	}

	// base64.StdEncoding ignores newlines per RFC 4648, so wrapped vs
	// unwrapped base64 from `base64` (BSD wraps at 76; GNU with -w0
	// doesn't) decodes the same way.
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimRight(string(res.Output), "\n"))
	if err != nil {
		return nil, fmt.Errorf("read: decode base64: %w", err)
	}
	return decoded, nil
}

// Write implements [backend.RemoteFS] with atomic semantics: bytes go to
// a tempfile, are sha256-verified (when sha256sum is available), and
// only then renamed into place.
func (b *Backend) Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	if len(data) > b.maxShellFileSize {
		return fmt.Errorf("%w: %d bytes (limit %d)",
			backend.ErrTooLarge, len(data), b.maxShellFileSize)
	}
	p, err := b.ensureProbed(ctx)
	if err != nil {
		return err
	}

	tempPath, err := tempPathFor(path)
	if err != nil {
		return err
	}

	// On any error, try to remove the tempfile. Best-effort — if the
	// remote is dying, we accept the leak.
	cleanup := true
	defer func() {
		if cleanup {
			_ = b.removeBestEffort(context.Background(), tempPath)
		}
	}()

	encoded := base64.StdEncoding.EncodeToString(data)

	// Chunk the encoded payload so the resulting shell command line
	// stays under typical ARG_MAX limits and the channel's chunked
	// write capacity.
	first := true
	for i := 0; i < len(encoded); i += b.chunkSize {
		end := i + b.chunkSize
		if end > len(encoded) {
			end = len(encoded)
		}
		piece := encoded[i:end]
		redirect := ">"
		if !first {
			redirect = ">>"
		}
		cmd := fmt.Sprintf("printf '%%s' %s | %s %s %s",
			shellquote.Quote(piece),
			p.b64DecodeCmd(),
			redirect,
			shellquote.Quote(tempPath),
		)
		if err := b.runMustSucceed(ctx, backend.OpWrite, cmd); err != nil {
			return err
		}
		first = false
	}
	if first {
		// data was empty — still create the tempfile.
		cmd := fmt.Sprintf(": > %s", shellquote.Quote(tempPath))
		if err := b.runMustSucceed(ctx, backend.OpWrite, cmd); err != nil {
			return err
		}
	}

	// Verify the bytes survived the trip.
	if p.Sha256Cmd != "" {
		want := sha256.Sum256(data)
		wantHex := hex.EncodeToString(want[:])
		cmd := fmt.Sprintf("%s -- %s | awk '{print $1}'",
			p.Sha256Cmd, shellquote.Quote(tempPath))
		res, err := b.runShell(ctx, cmd, nil)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return shellError(backend.OpWrite, res.ExitCode, res.Output)
		}
		gotHex := strings.TrimSpace(string(res.Output))
		if gotHex != wantHex {
			return fmt.Errorf("%w: want %s got %s",
				backend.ErrCorrupted, wantHex, gotHex)
		}
	}

	// Final chmod + atomic rename. We deliberately omit `--` here:
	// BSD/macOS chmod and mv don't recognize it, while GNU is fine
	// either way. Paths are shell-quoted so this stays safe.
	finalize := fmt.Sprintf("chmod %o %s && mv %s %s",
		mode.Perm(),
		shellquote.Quote(tempPath),
		shellquote.Quote(tempPath),
		shellquote.Quote(path),
	)
	if err := b.runMustSucceed(ctx, backend.OpWrite, finalize); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// OpenRead implements [backend.RemoteFS] as an in-memory streaming
// adapter — for v0.1.0 we still pull the whole file via Read but expose
// it as an io.ReadCloser so callers can switch to true streaming in
// v0.2.0 without API changes.
//
// Returns ErrTooLarge if the file exceeds MaxShellFileSize.
func (b *Backend) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	data, err := b.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// OpenWrite implements [backend.RemoteFS] as an in-memory buffering
// adapter — bytes are accumulated in RAM and committed via Write on
// Close. The contract matches a true streaming writer except the bytes
// don't reach the remote until Close.
//
// True streaming with sha256 deferred to v0.2.0; the API is stable.
func (b *Backend) OpenWrite(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	if _, err := b.ensureProbed(ctx); err != nil {
		return nil, err
	}
	return &bufferedWriter{
		ctx:  ctx,
		b:    b,
		path: path,
		mode: mode,
	}, nil
}

type bufferedWriter struct {
	ctx    context.Context
	b      *Backend
	path   string
	mode   fs.FileMode
	buf    []byte
	closed bool
}

func (w *bufferedWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("write: closed")
	}
	if len(w.buf)+len(p) > w.b.maxShellFileSize {
		return 0, fmt.Errorf("%w: stream exceeded %d bytes",
			backend.ErrTooLarge, w.b.maxShellFileSize)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *bufferedWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.b.Write(w.ctx, w.path, w.buf, w.mode)
}

// removeBestEffort tries to remove path but does not surface errors —
// used in failure-cleanup paths where we cannot do better.
func (b *Backend) removeBestEffort(ctx context.Context, path string) error {
	cmd := fmt.Sprintf("rm -f %s", shellquote.Quote(path))
	_, err := b.runShell(ctx, cmd, nil)
	return err
}

// tempPathFor returns a sibling path with a random suffix, suitable as a
// short-lived staging file.
func tempPathFor(path string) (string, error) {
	var n [8]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s.tmp.%s", path, hex.EncodeToString(n[:])), nil
}
