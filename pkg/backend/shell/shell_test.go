package shell_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// newBackend wires up a real bash-PTY-backed Session and a ShellBackend
// on top, returning both with a single cleanup hook.
func newBackend(t *testing.T) (*shell.Backend, func()) {
	t.Helper()
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	b := shell.New(sess)
	return b, func() { _ = sess.Close() }
}

// scratchDir returns a per-test directory under the OS temp area; it is
// NOT cleaned up automatically — the test should remove its own files.
// Using the host OS temp dir is fine because ShellBackend is talking
// over a PTY to a bash on the SAME machine.
func scratchDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "ptyrelay-shell-")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func TestProbe(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestRead_Simple(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "hello.txt")
	want := []byte("hello, ptyrelay\n")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := b.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRead_Binary(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "binary.bin")
	want := make([]byte, 8192)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := b.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("binary mismatch: %d bytes vs %d", len(got), len(want))
	}
}

func TestRead_TooLarge(t *testing.T) {
	t.Parallel()
	// 2 KiB cap so we can trip it cheaply.
	b, cleanup := newBackend(t)
	defer cleanup()
	b = shell.New(b.Session(), shell.WithMaxShellFileSize(2048))

	dir := scratchDir(t)
	path := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(path, make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := b.Read(ctx, path)
	if !errors.Is(err, backend.ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "round.txt")
	want := []byte("the quick brown fox\nover the lazy dog\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.Write(ctx, path, want, 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("contents mismatch: got %q want %q", got, want)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", st.Mode().Perm())
	}
}

func TestWrite_Binary_Chunked(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()
	// Force chunking — chunk size smaller than payload.
	b = shell.New(b.Session(), shell.WithChunkSize(4096))

	dir := scratchDir(t)
	path := filepath.Join(dir, "chunked.bin")
	want := make([]byte, 64*1024)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := b.Write(ctx, path, want, 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("64KiB roundtrip mismatch")
	}
}

func TestWrite_AtomicTempfileCleanup(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	// Write to a path whose parent doesn't exist — the chmod/mv step
	// should fail and the tempfile (in the same parent) shouldn't
	// remain.
	path := filepath.Join(dir, "missing-parent", "out.txt")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := b.Write(ctx, path, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("expected error for path with missing parent")
	}

	// Tempfile (if any) lived under "missing-parent/" which doesn't
	// exist, so we can't directly check; instead verify no `out.txt`
	// landed.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("partial file should not exist; stat err=%v", err)
	}
}

func TestStat(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "stat-target")
	if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := b.Stat(ctx, path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 5 {
		t.Errorf("size = %d, want 5", info.Size)
	}
	if info.IsDir {
		t.Error("IsDir = true, want false")
	}
	if info.Mode.Perm() != 0o644 {
		t.Errorf("mode = %o, want 0644", info.Mode.Perm())
	}
}

func TestStat_Directory(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := b.Stat(ctx, dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir {
		t.Error("IsDir = false, want true")
	}
}

func TestLstat_Symlink(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := b.Lstat(ctx, link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !info.IsSymlink {
		t.Error("IsSymlink = false, want true")
	}
	if info.SymlinkTarget != target {
		t.Errorf("target = %q, want %q", info.SymlinkTarget, target)
	}

	// Stat (follow) should land on the regular file.
	info2, err := b.Stat(ctx, link)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info2.IsSymlink {
		t.Error("Stat result IsSymlink=true; want followed")
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma"} {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	entries, err := b.List(ctx, dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	names := make(map[string]backend.FileInfo)
	for _, e := range entries {
		names[e.Name] = e
	}
	for _, want := range []string{"alpha.txt", "beta.txt", "gamma", "sub"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing entry %q (got %v)", want, entries)
		}
	}
	if !names["sub"].IsDir {
		t.Error("sub should be IsDir")
	}
}

func TestMkdirAll_RenameRemove(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	deep := filepath.Join(dir, "a", "b", "c")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.MkdirAll(ctx, deep, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if st, err := os.Stat(deep); err != nil || !st.IsDir() {
		t.Fatalf("deep dir not created: %v / %v", st, err)
	}

	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Rename(ctx, src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("after rename: %v", err)
	}
	if err := b.Remove(ctx, dst); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("after remove: stat err = %v", err)
	}
}

func TestRun_StdoutAndExitCode(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := b.Run(ctx, "echo result; (exit 3)", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "result") {
		t.Errorf("stdout = %q, want to contain 'result'", res.Stdout)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestRun_Stdin(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := b.Run(ctx, "tr a-z A-Z", []byte("hello"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "HELLO") {
		t.Errorf("stdout = %q, want to contain 'HELLO'", res.Stdout)
	}
}

func TestOpenRead_Streaming(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "stream.txt")
	want := []byte("streaming content goes here")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rc, err := b.OpenRead(ctx, path)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOpenWrite_Streaming(t *testing.T) {
	t.Parallel()
	b, cleanup := newBackend(t)
	defer cleanup()

	dir := scratchDir(t)
	path := filepath.Join(dir, "out-stream.txt")
	want := []byte("buffered streaming write")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	wc, err := b.OpenWrite(ctx, path, 0o644)
	if err != nil {
		t.Fatalf("OpenWrite: %v", err)
	}
	if _, err := wc.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
