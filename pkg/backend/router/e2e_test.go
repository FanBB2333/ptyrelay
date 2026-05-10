package router_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/router"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/bootstrap"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// TestE2E_FullStack walks the v0.2.0 happy path end to end:
// PTY → Channel → Session → ShellBackend → Bootstrap → AgentBackend
// → RouterBackend, then exercises the routing rules under failure.
//
// This is the "does v0.2.0 actually work" smoke test. If it fails
// after a refactor, the integration is broken.
func TestE2E_FullStack(t *testing.T) {
	t.Parallel()

	// --- Stage 1: build the agent for the host platform.
	provDir := t.TempDir()
	agentBuildPath := filepath.Join(provDir, runtime.GOOS+"-"+runtime.GOARCH)
	build := exec.Command("go", "build", "-o", agentBuildPath, "./cmd/ptyrelay-agent")
	build.Dir = repoRoot(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, out)
	}
	provider := &bootstrap.FileProvider{Dir: provDir}

	// --- Stage 2: open a PTY-backed shell session and a ShellBackend.
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// --- Stage 3: bootstrap the agent through the shell.
	installPath := filepath.Join(t.TempDir(), "ptyrelay-agent")
	got, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		Provider:    provider,
		InstallPath: installPath,
	})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got != installPath {
		t.Fatalf("install path mismatch: %q vs %q", got, installPath)
	}
	if err := bootstrap.VerifyInstall(ctx, sb, installPath); err != nil {
		t.Fatalf("VerifyInstall: %v", err)
	}

	// --- Stage 4: build a Router on top of the agent + shell pair.
	ab := agent.New(sess, installPath)
	rb := router.New(ab, sb)
	if err := rb.Probe(ctx); err != nil {
		t.Fatalf("Router probe: %v", err)
	}
	if !rb.AgentHealthy() {
		t.Fatal("agent should be healthy after bootstrap")
	}

	// --- Stage 5: write a non-trivial binary file via the Router and
	// read it back. Exercises the agent's chunked-staging path
	// (request payload > MAX_INPUT) and round-trip integrity.
	scratch := t.TempDir()
	bigPath := filepath.Join(scratch, "agent-roundtrip.bin")
	want := make([]byte, 8192)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := rb.Write(ctx, bigPath, want, 0o600); err != nil {
		t.Fatalf("rb.Write: %v", err)
	}
	got2, err := rb.Read(ctx, bigPath)
	if err != nil {
		t.Fatalf("rb.Read: %v", err)
	}
	if !bytes.Equal(got2, want) {
		t.Errorf("8 KiB roundtrip mismatch")
	}

	// --- Stage 6: prove the agent path is active by checking stderr
	// separation. ShellBackend can't separate stdout/stderr (PTY
	// merges them); AgentBackend does. Clean stderr means we routed
	// through the agent.
	res, err := rb.Run(ctx, "echo to-stdout; echo to-stderr 1>&2", nil)
	if err != nil {
		t.Fatalf("rb.Run: %v", err)
	}
	if !bytes.Contains(res.Stderr, []byte("to-stderr")) {
		t.Errorf("expected stderr separation (agent path), got stdout=%q stderr=%q",
			res.Stdout, res.Stderr)
	}
	if bytes.Contains(res.Stdout, []byte("to-stderr")) {
		t.Errorf("stderr leaked into stdout: %q", res.Stdout)
	}

	// --- Stage 7: kill the agent binary mid-flight. ReadOnly ops
	// should silently fall back to the shell.
	if err := os.Remove(installPath); err != nil {
		t.Fatal(err)
	}
	got3, err := rb.Read(ctx, bigPath)
	if err != nil {
		t.Fatalf("rb.Read after agent dies: %v", err)
	}
	if !bytes.Equal(got3, want) {
		t.Errorf("post-fallback read mismatch")
	}
	if rb.AgentHealthy() {
		t.Error("agent should be marked unhealthy after the failed call")
	}

	// --- Stage 8: re-bootstrap and re-probe — Router should adopt
	// the agent again on the next Probe.
	if _, err := bootstrap.Bootstrap(ctx, sb, bootstrap.Options{
		Provider:    provider,
		InstallPath: installPath,
	}); err != nil {
		t.Fatalf("re-Bootstrap: %v", err)
	}
	if err := rb.Probe(ctx); err != nil {
		t.Fatalf("re-Probe: %v", err)
	}
	if !rb.AgentHealthy() {
		t.Error("agent should be healthy after re-Probe")
	}
}
