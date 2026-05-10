package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

// dispatch routes one request to the right handler and constructs the
// response envelope. Handlers return either a typed result (on ok) or an
// error (which is mapped to ErrKind here, in one place).
func dispatch(req *proto.Request) *proto.Response {
	if req.V != proto.Version {
		return errorResponse(req.ID, proto.ErrKindBadProto,
			fmt.Errorf("unsupported wire version %d (want %d)", req.V, proto.Version))
	}

	type handler func(args json.RawMessage) (any, error)
	handlers := map[proto.Op]handler{
		proto.OpPing:     handlePing,
		proto.OpRead:     handleRead,
		proto.OpWrite:    handleWrite,
		proto.OpStat:     handleStat,
		proto.OpLstat:    handleLstat,
		proto.OpList:     handleList,
		proto.OpRemove:   handleRemove,
		proto.OpRename:   handleRename,
		proto.OpMkdirAll: handleMkdirAll,
		proto.OpRun:      handleRun,
		proto.OpBye:      handleBye,
	}
	h, ok := handlers[req.Op]
	if !ok {
		return errorResponse(req.ID, proto.ErrKindUnknownOp,
			fmt.Errorf("unknown op %q", req.Op))
	}
	data, err := h(req.Args)
	if err != nil {
		return errorResponse(req.ID, classify(err), err)
	}
	return okResponse(req.ID, data)
}

func okResponse(id string, data any) *proto.Response {
	resp := &proto.Response{V: proto.Version, ID: id, OK: true}
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return errorResponse(id, proto.ErrKindInternal, err)
		}
		resp.Data = raw
	}
	return resp
}

func errorResponse(id, kind string, err error) *proto.Response {
	return &proto.Response{
		V:       proto.Version,
		ID:      id,
		OK:      false,
		Err:     err.Error(),
		ErrKind: kind,
	}
}

// classify maps a Go error to a proto.ErrKind* category. The mapping is
// best-effort — unknown errors fall through to ErrKindIO.
func classify(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return proto.ErrKindNotFound
	case errors.Is(err, os.ErrPermission):
		return proto.ErrKindPermission
	default:
		return proto.ErrKindIO
	}
}

// ----- handlers -----

func handlePing(_ json.RawMessage) (any, error) {
	return &proto.PingData{
		Version:      proto.Version,
		AgentVersion: proto.AgentVersion + " " + runtime.GOOS + "/" + runtime.GOARCH,
		Caps:         []string{"v1", "one-shot", "repl"},
	}, nil
}

func handleBye(_ json.RawMessage) (any, error) {
	return nil, nil
}

func handleRead(raw json.RawMessage) (any, error) {
	var args proto.ReadArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(args.Path)
	if err != nil {
		return nil, err
	}
	return &proto.ReadData{Bytes: data}, nil
}

func handleWrite(raw json.RawMessage) (any, error) {
	var args proto.WriteArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	// Atomic write: tempfile in same dir, then rename. fs.FileMode is
	// applied via os.OpenFile + chmod (umask doesn't bite).
	dir, base := filepath.Split(args.Path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, base+".tmp.*")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(args.Bytes); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(args.Mode.Perm()); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), args.Path); err != nil {
		return nil, err
	}
	cleanup = false
	return nil, nil
}

func handleStat(raw json.RawMessage) (any, error) {
	return statImpl(raw, true)
}

func handleLstat(raw json.RawMessage) (any, error) {
	return statImpl(raw, false)
}

func statImpl(raw json.RawMessage, follow bool) (any, error) {
	var args proto.StatArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	var info fs.FileInfo
	var err error
	if follow {
		info, err = os.Stat(args.Path)
	} else {
		info, err = os.Lstat(args.Path)
	}
	if err != nil {
		return nil, err
	}
	out := fileInfoTo(info, args.Path)
	if !follow && out.IsSymlink {
		if target, terr := os.Readlink(args.Path); terr == nil {
			out.SymlinkTarget = target
		}
	}
	return out, nil
}

func handleList(raw json.RawMessage) (any, error) {
	var args proto.ListArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(args.Path)
	if err != nil {
		return nil, err
	}
	out := &proto.ListData{Entries: make([]proto.StatData, 0, len(entries))}
	for _, e := range entries {
		full := filepath.Join(args.Path, e.Name())
		// Use Lstat so symlinks are reported as symlinks rather than
		// followed — matches the contract of RemoteFS.List.
		fi, err := os.Lstat(full)
		if err != nil {
			continue // tolerate races/perms; List is best-effort per entry
		}
		sd := *fileInfoTo(fi, full)
		if sd.IsSymlink {
			if target, terr := os.Readlink(full); terr == nil {
				sd.SymlinkTarget = target
			}
		}
		out.Entries = append(out.Entries, sd)
	}
	// Stable order makes tests deterministic.
	sort.Slice(out.Entries, func(i, j int) bool {
		return out.Entries[i].Name < out.Entries[j].Name
	})
	return out, nil
}

func handleRemove(raw json.RawMessage) (any, error) {
	var args proto.RemoveArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := os.Remove(args.Path); err != nil {
		return nil, err
	}
	return nil, nil
}

func handleRename(raw json.RawMessage) (any, error) {
	var args proto.RenameArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := os.Rename(args.OldPath, args.NewPath); err != nil {
		return nil, err
	}
	return nil, nil
}

func handleMkdirAll(raw json.RawMessage) (any, error) {
	var args proto.MkdirAllArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(args.Path, args.Mode.Perm()); err != nil {
		return nil, err
	}
	// MkdirAll uses umask for intermediate dirs; force the leaf to
	// the requested mode.
	if err := os.Chmod(args.Path, args.Mode.Perm()); err != nil {
		return nil, err
	}
	return nil, nil
}

func handleRun(raw json.RawMessage) (any, error) {
	var args proto.RunArgs
	if err := unmarshalArgs(raw, &args); err != nil {
		return nil, err
	}
	cmd := exec.Command("sh", "-c", args.Cmd)
	if len(args.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(args.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	exitCode := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			// A non-exit failure (e.g. cannot fork) should propagate
			// as a real error rather than being mistaken for an exit
			// code 1.
			return nil, runErr
		}
	}
	return &proto.RunData{
		Stdout:   stdout.Bytes(),
		Stderr:   stderr.Bytes(),
		ExitCode: exitCode,
		Duration: dur,
	}, nil
}

// fileInfoTo translates fs.FileInfo into the wire shape.
func fileInfoTo(info fs.FileInfo, path string) *proto.StatData {
	mode := info.Mode()
	out := &proto.StatData{
		Name:      info.Name(),
		Size:      info.Size(),
		Mode:      uint32(mode),
		ModTime:   info.ModTime(),
		IsDir:     info.IsDir(),
		IsSymlink: mode&fs.ModeSymlink != 0,
	}
	_ = path // reserved for future when callers want the canonical absolute path
	return out
}

// unmarshalArgs decodes a request's args field into v, returning a
// classified bad-args error if decoding fails.
func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("missing args")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad args: %w", err)
	}
	return nil
}
