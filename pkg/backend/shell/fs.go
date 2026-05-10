package shell

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
	"github.com/FanBB2333/ptyrelay/pkg/backend"
)

// Stat implements [backend.RemoteFS]; follows symlinks.
func (b *Backend) Stat(ctx context.Context, path string) (*backend.FileInfo, error) {
	return b.statImpl(ctx, path, true)
}

// Lstat implements [backend.RemoteFS]; does not follow symlinks. If path
// is a symlink, the result has IsSymlink=true and SymlinkTarget set.
func (b *Backend) Lstat(ctx context.Context, path string) (*backend.FileInfo, error) {
	return b.statImpl(ctx, path, false)
}

func (b *Backend) statImpl(ctx context.Context, path string, follow bool) (*backend.FileInfo, error) {
	p, err := b.ensureProbed(ctx)
	if err != nil {
		return nil, err
	}

	cmd := buildStatCmd(p, path, follow)
	res, err := b.runShell(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, shellError(backend.OpStat, res.ExitCode, res.Output)
	}

	info, err := parseStatLine(p.StatStyle, strings.TrimSpace(string(res.Output)), path)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if !follow && info.IsSymlink {
		target, terr := b.readlink(ctx, path)
		if terr == nil {
			info.SymlinkTarget = target
		}
	}
	return info, nil
}

func buildStatCmd(p *probeResult, path string, follow bool) string {
	q := shellquote.Quote(path)
	switch p.StatStyle {
	case "gnu":
		flag := ""
		if follow {
			flag = "-L "
		}
		return fmt.Sprintf("stat %s-c '%%s|%%Y|%%a|%%F' -- %s", flag, q)
	default: // bsd
		flag := ""
		if follow {
			flag = "-L "
		}
		// %z size, %m mtime, %Lp permission bits (octal), %HT type
		return fmt.Sprintf("stat %s-f '%%z|%%m|%%Lp|%%HT' %s", flag, q)
	}
}

// parseStatLine accepts the output of buildStatCmd and returns a FileInfo.
//
// GNU format: "<size>|<mtime>|<mode-octal>|<file-type-text>"
// BSD format: "<size>|<mtime>|<mode-octal>|<file-type-text>"
//
// We pinned both to the same field order to keep the parser unified.
func parseStatLine(style, line, path string) (*backend.FileInfo, error) {
	parts := strings.Split(line, "|")
	if len(parts) < 4 {
		return nil, fmt.Errorf("malformed stat output: %q", line)
	}
	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("size: %w", err)
	}
	mtimeSec, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return nil, fmt.Errorf("mtime: %w", err)
	}
	modeOct, err := strconv.ParseUint(parts[2], 8, 32)
	if err != nil {
		return nil, fmt.Errorf("mode: %w", err)
	}
	typeText := parts[3]

	mode := fs.FileMode(modeOct & 0o7777)
	isDir := false
	isLink := false
	switch style {
	case "gnu":
		switch typeText {
		case "directory":
			isDir = true
			mode |= fs.ModeDir
		case "symbolic link":
			isLink = true
			mode |= fs.ModeSymlink
		case "regular file", "regular empty file":
			// no extra bits
		}
	default:
		// BSD: %HT yields "Directory", "Symbolic Link", "Regular File", ...
		switch typeText {
		case "Directory":
			isDir = true
			mode |= fs.ModeDir
		case "Symbolic Link":
			isLink = true
			mode |= fs.ModeSymlink
		case "Regular File":
			// no extra bits
		}
	}

	return &backend.FileInfo{
		Name:      lastPathSegment(path),
		Size:      size,
		Mode:      mode,
		ModTime:   time.Unix(int64(mtimeSec), int64((mtimeSec-float64(int64(mtimeSec)))*1e9)),
		IsDir:     isDir,
		IsSymlink: isLink,
	}, nil
}

func (b *Backend) readlink(ctx context.Context, path string) (string, error) {
	cmd := fmt.Sprintf("readlink -- %s", shellquote.Quote(path))
	res, err := b.runShell(ctx, cmd, nil)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("readlink: exit %d", res.ExitCode)
	}
	return strings.TrimRight(string(res.Output), "\n"), nil
}

// List implements [backend.RemoteFS].
//
// We use `find … -print0 | xargs -0 stat …` so filenames with spaces and
// shell metacharacters are handled exactly. -print0 is GNU+BSD; xargs -0
// is GNU+BSD; the stat format string is selected by the probe.
func (b *Backend) List(ctx context.Context, path string) ([]backend.FileInfo, error) {
	p, err := b.ensureProbed(ctx)
	if err != nil {
		return nil, err
	}

	q := shellquote.Quote(path)
	var statFmt string
	switch p.StatStyle {
	case "gnu":
		// %n=name, %s=size, %Y=mtime, %a=mode (octal), %F=type
		statFmt = "stat -c '%n|%s|%Y|%a|%F' --"
	default:
		statFmt = "stat -f '%N|%z|%m|%Lp|%HT'"
	}
	cmd := fmt.Sprintf("find %s -mindepth 1 -maxdepth 1 -print0 2>/dev/null | xargs -0 -n 32 %s", q, statFmt)

	res, err := b.runShell(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, shellError(backend.OpList, res.ExitCode, res.Output)
	}

	var entries []backend.FileInfo
	for _, line := range strings.Split(strings.TrimRight(string(res.Output), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// list lines carry the full path in the first field; strip
		// off the directory prefix to get the entry name.
		parts := strings.SplitN(line, "|", 5)
		if len(parts) < 5 {
			return nil, fmt.Errorf("malformed list line: %q", line)
		}
		fullPath := parts[0]
		// rebuild a stat-line shape (drop the path field) and reuse
		// parseStatLine.
		statLine := strings.Join(parts[1:], "|")
		info, perr := parseStatLine(p.StatStyle, statLine, fullPath)
		if perr != nil {
			return nil, perr
		}
		entries = append(entries, *info)
	}
	return entries, nil
}

// MkdirAll implements [backend.RemoteFS].
func (b *Backend) MkdirAll(ctx context.Context, path string, mode fs.FileMode) error {
	if _, err := b.ensureProbed(ctx); err != nil {
		return err
	}
	q := shellquote.Quote(path)
	// Two-step: mkdir -p creates the chain, chmod sets the mode on
	// the final leaf. Intermediate dirs keep the umask default — the
	// alternative (chmod -R) would clobber pre-existing permissions
	// on parents. `--` is omitted for BSD/macOS portability.
	cmd := fmt.Sprintf("mkdir -p %s && chmod %o %s", q, mode.Perm(), q)
	if err := b.runMustSucceed(ctx, backend.OpMkdirAll, cmd); err != nil {
		return err
	}
	return nil
}

// Rename implements [backend.RemoteFS].
func (b *Backend) Rename(ctx context.Context, oldPath, newPath string) error {
	if _, err := b.ensureProbed(ctx); err != nil {
		return err
	}
	cmd := fmt.Sprintf("mv %s %s", shellquote.Quote(oldPath), shellquote.Quote(newPath))
	return b.runMustSucceed(ctx, backend.OpRename, cmd)
}

// Remove implements [backend.RemoteFS].
//
// Refuses to descend directories — recursive removal is dangerous and
// belongs to a dedicated RemoveAll op (out of scope for v0.1.0).
func (b *Backend) Remove(ctx context.Context, path string) error {
	if _, err := b.ensureProbed(ctx); err != nil {
		return err
	}
	cmd := fmt.Sprintf("rm -f %s", shellquote.Quote(path))
	return b.runMustSucceed(ctx, backend.OpRemove, cmd)
}

// runMustSucceed runs cmd and returns shellError if it exits non-zero.
func (b *Backend) runMustSucceed(ctx context.Context, op backend.Op, cmd string) error {
	res, err := b.runShell(ctx, cmd, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return shellError(op, res.ExitCode, res.Output)
	}
	return nil
}

func lastPathSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
