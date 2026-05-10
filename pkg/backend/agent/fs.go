package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

// Read implements [backend.RemoteFS].
func (b *Backend) Read(ctx context.Context, path string) ([]byte, error) {
	var data proto.ReadData
	if err := b.callOp(ctx, proto.OpRead, proto.ReadArgs{Path: path}, &data); err != nil {
		return nil, err
	}
	return data.Bytes, nil
}

// Write implements [backend.RemoteFS]. The agent does atomic write
// (tempfile + rename) on the remote so retries are safe.
func (b *Backend) Write(ctx context.Context, path string, data []byte, mode fs.FileMode) error {
	return b.callOp(ctx, proto.OpWrite, proto.WriteArgs{
		Path:  path,
		Bytes: data,
		Mode:  mode,
	}, nil)
}

// Stat implements [backend.RemoteFS]; follows symlinks.
func (b *Backend) Stat(ctx context.Context, path string) (*backend.FileInfo, error) {
	return b.statImpl(ctx, proto.OpStat, path)
}

// Lstat implements [backend.RemoteFS]; does not follow symlinks.
func (b *Backend) Lstat(ctx context.Context, path string) (*backend.FileInfo, error) {
	return b.statImpl(ctx, proto.OpLstat, path)
}

func (b *Backend) statImpl(ctx context.Context, op proto.Op, path string) (*backend.FileInfo, error) {
	var data proto.StatData
	if err := b.callOp(ctx, op, proto.StatArgs{Path: path}, &data); err != nil {
		return nil, err
	}
	return statDataToFileInfo(&data), nil
}

// List implements [backend.RemoteFS].
func (b *Backend) List(ctx context.Context, path string) ([]backend.FileInfo, error) {
	var data proto.ListData
	if err := b.callOp(ctx, proto.OpList, proto.ListArgs{Path: path}, &data); err != nil {
		return nil, err
	}
	out := make([]backend.FileInfo, len(data.Entries))
	for i := range data.Entries {
		out[i] = *statDataToFileInfo(&data.Entries[i])
	}
	return out, nil
}

// MkdirAll implements [backend.RemoteFS].
func (b *Backend) MkdirAll(ctx context.Context, path string, mode fs.FileMode) error {
	return b.callOp(ctx, proto.OpMkdirAll, proto.MkdirAllArgs{Path: path, Mode: mode}, nil)
}

// Rename implements [backend.RemoteFS].
func (b *Backend) Rename(ctx context.Context, oldPath, newPath string) error {
	return b.callOp(ctx, proto.OpRename, proto.RenameArgs{OldPath: oldPath, NewPath: newPath}, nil)
}

// Remove implements [backend.RemoteFS].
func (b *Backend) Remove(ctx context.Context, path string) error {
	return b.callOp(ctx, proto.OpRemove, proto.RemoveArgs{Path: path}, nil)
}

// OpenRead implements [backend.RemoteFS] as a buffered adapter — like
// ShellBackend, true streaming joins in v0.3.0 once Pipe + a chunked
// op are in place.
func (b *Backend) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	data, err := b.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// OpenWrite implements [backend.RemoteFS] as a buffered adapter.
func (b *Backend) OpenWrite(ctx context.Context, path string, mode fs.FileMode) (io.WriteCloser, error) {
	return &bufferedWriter{ctx: ctx, b: b, path: path, mode: mode}, nil
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
		return 0, fmt.Errorf("agent: write after close")
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

func statDataToFileInfo(s *proto.StatData) *backend.FileInfo {
	return &backend.FileInfo{
		Name:          s.Name,
		Size:          s.Size,
		Mode:          fs.FileMode(s.Mode),
		ModTime:       s.ModTime,
		IsDir:         s.IsDir,
		IsSymlink:     s.IsSymlink,
		SymlinkTarget: s.SymlinkTarget,
	}
}
