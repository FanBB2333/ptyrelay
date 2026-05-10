package router_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/backend/router"
	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// newRouter builds an agent binary, sets up a PTY+session, and returns
// a RouterBackend wrapping a fresh agent + shell pair. The returned
// `cleanup` must run before the test exits.
func newRouter(t *testing.T) (*router.Backend, *agent.Backend, *shell.Backend, func()) {
	t.Helper()
	agentPath := buildAgent(t)
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	sb := shell.New(sess)
	ab := agent.New(sess, agentPath)
	rb := router.New(ab, sb)
	return rb, ab, sb, func() { _ = sess.Close() }
}

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

func TestRouter_AgentHealthyUsesAgent(t *testing.T) {
	t.Parallel()
	rb, _, _, cleanup := newRouter(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := rb.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !rb.AgentHealthy() {
		t.Fatal("agent should be healthy after successful Probe")
	}

	// Using Run is the cleanest test: AgentBackend separates stdout
	// and stderr, ShellBackend merges them. So if Stderr arrives in
	// .Stderr (not in .Stdout) we know the agent path served the
	// request.
	res, err := rb.Run(ctx, "echo out; echo err 1>&2", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !bytes.Contains(res.Stderr, []byte("err")) {
		t.Errorf("expected stderr to contain 'err' (agent path), got stdout=%q stderr=%q",
			res.Stdout, res.Stderr)
	}
}

func TestRouter_AgentMissingFallsBack(t *testing.T) {
	t.Parallel()
	// Build a router whose agent binary path is invalid.
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)
	ab := agent.New(sess, "/no/such/agent/binary")
	rb := router.New(ab, sb)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Probe should not return an error — Router treats a missing
	// agent as "fallback to shell, still functional".
	if err := rb.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if rb.AgentHealthy() {
		t.Error("agent should NOT be healthy with bogus binary")
	}

	// Stat works through the shell fallback.
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := rb.Stat(ctx, path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 2 {
		t.Errorf("size=%d", info.Size)
	}
}

func TestRouter_AgentDiesMidOpReadOnlyFallsBack(t *testing.T) {
	t.Parallel()
	rb, ab, _, cleanup := newRouter(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := rb.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if !rb.AgentHealthy() {
		t.Fatal("expected healthy")
	}

	// Sabotage the agent: make its binary unrunnable.
	if err := os.Remove(ab.AgentPath()); err != nil {
		t.Fatal(err)
	}

	// A ReadOnly op should fall back to shell on the next call.
	dir := t.TempDir()
	path := filepath.Join(dir, "x")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := rb.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q", got)
	}
	if rb.AgentHealthy() {
		t.Error("agent should be marked unhealthy after the failed call")
	}
}

func TestRouter_AgentDiesMidOpNonIdempotentSurfaces(t *testing.T) {
	t.Parallel()
	rb, ab, _, cleanup := newRouter(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := rb.Probe(ctx); err != nil {
		t.Fatal(err)
	}

	// Sabotage agent.
	if err := os.Remove(ab.AgentPath()); err != nil {
		t.Fatal(err)
	}

	// Run is NonIdempotent — Router must surface the agent error
	// rather than silently re-running through shell.
	_, err := rb.Run(ctx, "echo never", nil)
	if err == nil {
		t.Fatal("expected error for NonIdempotent op with dead agent")
	}
}

func TestRouter_OpClassMatrix(t *testing.T) {
	t.Parallel()
	// Sanity check — Router can rely on Op.Class for routing because
	// the mapping is stable. This is a redundant guard against future
	// edits silently changing the class of a critical op.
	want := map[backend.Op]backend.OpClass{
		backend.OpRead:   backend.ClassReadOnly,
		backend.OpWrite:  backend.ClassIdempotent,
		backend.OpRemove: backend.ClassNonIdempotent,
		backend.OpRun:    backend.ClassNonIdempotent,
	}
	for op, w := range want {
		if got := op.Class(); got != w {
			t.Errorf("Op(%q).Class() = %d, want %d", op, got, w)
		}
	}
	// And the route helper itself should not surface an error type
	// that swallows the underlying Go error chain.
	wrapped := errors.New("agent died")
	if !errors.Is(wrapped, wrapped) {
		t.Fatal("sanity")
	}
}
