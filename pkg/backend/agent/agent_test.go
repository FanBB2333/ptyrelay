package agent_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// buildAgent compiles cmd/ptyrelay-agent into the test's temp dir and
// returns the absolute path. Tests use the host machine as the
// "remote", so just running the binary by absolute path is enough.
func buildAgent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "ptyrelay-agent")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/ptyrelay-agent")
	cmd.Dir = repoRoot(t)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build agent: %v\n%s", err, buildOut)
	}
	return out
}

// repoRoot finds the module root by walking up from the test's cwd.
func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := cwd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("could not locate go.mod")
	return ""
}

func newAgentBackend(t *testing.T) (*agent.Backend, func()) {
	t.Helper()
	agentPath := buildAgent(t)
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	be := agent.New(sess, agentPath)
	return be, func() { _ = sess.Close() }
}

func TestAgent_Probe(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := be.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestAgent_Read_Write_Roundtrip(t *testing.T) {
	for _, size := range []int{256, 1024, 2048, 3000} {
		size := size
		t.Run(fmt.Sprintf("size=%d", size), func(t *testing.T) {
			t.Parallel()
			be, cleanup := newAgentBackend(t)
			defer cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			dir := t.TempDir()
			path := filepath.Join(dir, "rt.bin")
			want := make([]byte, size)
			if _, err := rand.Read(want); err != nil {
				t.Fatal(err)
			}

			if err := be.Write(ctx, path, want, 0o600); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, err := be.Read(ctx, path)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("contents differ (%d vs %d)", len(got), len(want))
			}
		})
	}
}

func TestAgent_Read_NotFound(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := be.Read(ctx, "/no/such/file/anywhere")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want errors.Is(os.ErrNotExist)", err)
	}
}

func TestAgent_Stat_Lstat_Symlink(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	li, err := be.Lstat(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if !li.IsSymlink {
		t.Error("Lstat: IsSymlink=false")
	}
	if li.SymlinkTarget != target {
		t.Errorf("Lstat: target=%q", li.SymlinkTarget)
	}

	si, err := be.Stat(ctx, link)
	if err != nil {
		t.Fatal(err)
	}
	if si.IsSymlink {
		t.Error("Stat: IsSymlink=true (should follow)")
	}
}

func TestAgent_List(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	_ = os.Mkdir(filepath.Join(dir, "sub"), 0o755)

	entries, err := be.List(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = e.IsDir
	}
	for _, want := range []string{"a", "b", "c", "sub"} {
		if _, ok := names[want]; !ok {
			t.Errorf("missing %q", want)
		}
	}
	if !names["sub"] {
		t.Errorf("sub should be IsDir")
	}
}

func TestAgent_MkdirAll_Rename_Remove(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	deep := filepath.Join(dir, "x", "y", "z")
	if err := be.MkdirAll(ctx, deep, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	src := filepath.Join(dir, "s")
	dst := filepath.Join(dir, "d")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if err := be.Rename(ctx, src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := be.Remove(ctx, dst); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestAgent_Run_StdoutStderrSeparated(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := be.Run(ctx, `echo out; echo err 1>&2; exit 5`, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Headline win over ShellBackend: stdout and stderr arrive on
	// separate streams.
	if !strings.Contains(string(res.Stdout), "out") {
		t.Errorf("stdout=%q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "err") {
		t.Errorf("stderr=%q", res.Stderr)
	}
	if strings.Contains(string(res.Stdout), "err") {
		t.Errorf("stderr leaked into stdout: %q", res.Stdout)
	}
	if res.ExitCode != 5 {
		t.Errorf("exit=%d", res.ExitCode)
	}
}

func TestAgent_Run_Stdin(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := be.Run(ctx, "tr a-z A-Z", []byte("ptyrelay agent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "PTYRELAY AGENT") {
		t.Errorf("stdout=%q", res.Stdout)
	}
}

func TestAgent_OpenWrite_OpenRead(t *testing.T) {
	t.Parallel()
	be, cleanup := newAgentBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "stream")
	want := []byte("buffered streaming")

	wc, err := be.OpenWrite(ctx, path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wc.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := wc.Close(); err != nil {
		t.Fatal(err)
	}

	rc, err := be.OpenRead(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}
