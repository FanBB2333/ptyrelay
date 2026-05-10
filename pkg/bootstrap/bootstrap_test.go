package bootstrap_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// buildAgentForHost compiles cmd/ptyrelay-agent into a directory laid
// out the way FileProvider expects — `<dir>/<os>-<arch>` — and returns
// that dir.
func buildAgentForHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, runtime.GOOS+"-"+runtime.GOARCH)
	// -s -w strips the symbol table; ~halves the test binary (3.4 MB → ~2 MB)
	// which makes the chunked upload through the PTY noticeably faster.
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", out, "./cmd/ptyrelay-agent")
	cmd.Dir = repoRoot(t)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build agent: %v\n%s", err, buildOut)
	}
	return dir
}

func repoRoot(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	for d := cwd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	t.Fatal("go.mod not found")
	return ""
}

func TestBootstrap_EndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("bootstrap: skipping multi-MB PTY upload integration test under -short")
	}
	t.Parallel()

	provider := &bootstrap.FileProvider{Dir: buildAgentForHost(t)}

	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	// Generous timeout — uploading a multi-MB binary through the PTY
	// in 32 KiB framed chunks is slow under -race (typically ~14×
	// non-race wallclock when other PTY-bound tests are also live).
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	installDir := t.TempDir()
	installPath := filepath.Join(installDir, "ptyrelay-agent")

	got, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		Provider:    provider,
		InstallPath: installPath,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got != installPath {
		t.Errorf("install path = %q, want %q", got, installPath)
	}

	// File exists, executable, non-empty.
	st, err := os.Stat(installPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size() == 0 {
		t.Error("agent binary is empty")
	}
	if st.Mode().Perm()&0o100 == 0 {
		t.Errorf("agent binary not executable, mode = %o", st.Mode().Perm())
	}

	// And actually pings.
	if err := bootstrap.VerifyInstall(ctx, sb, installPath); err != nil {
		t.Fatalf("VerifyInstall: %v", err)
	}

	// Sanity: AgentBackend talks to it directly.
	ab := agent.New(sess, installPath)
	if err := ab.Probe(ctx); err != nil {
		t.Fatalf("agent Probe via bootstrap path: %v", err)
	}
}

func TestBootstrap_UnsupportedTarget(t *testing.T) {
	t.Parallel()

	// Provider only knows about a fake target; the real host isn't there.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan9-amd64"), []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &bootstrap.FileProvider{Dir: dir}

	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		Provider:    provider,
		InstallPath: filepath.Join(t.TempDir(), "agent"),
	})
	if err == nil {
		t.Fatal("expected error for missing host binary")
	}
}
