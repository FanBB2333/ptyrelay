package agent_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

func newREPLBackend(t *testing.T) (*agent.Backend, func()) {
	t.Helper()
	agentPath := buildAgent(t)
	ch := testpty.NewBash(t)
	sess := session.New(ch, session.ShellBash)
	be := agent.New(sess, agentPath, agent.WithMode(agent.ModeREPL))
	return be, func() {
		_ = be.Close() // sends bye + waits
		_ = sess.Close()
	}
}

func TestAgentREPL_Probe(t *testing.T) {
	t.Parallel()
	be, cleanup := newREPLBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := be.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestAgentREPL_MultipleOpsOneProcess(t *testing.T) {
	t.Parallel()
	be, cleanup := newREPLBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	want := []byte("repl-mode")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	// Three sequential ops should all go through the SAME agent
	// process. The behavioral assertion is a wallclock one — REPL
	// is materially faster than one-shot's per-op fork. We don't
	// assert timings (flaky), but we assert correctness across
	// several ops.
	for i := 0; i < 3; i++ {
		got, err := be.Read(ctx, path)
		if err != nil {
			t.Fatalf("Read[%d]: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("Read[%d] = %q, want %q", i, got, want)
		}
	}

	res, err := be.Run(ctx, "echo from-repl", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(res.Stdout, []byte("from-repl")) {
		t.Errorf("stdout=%q", res.Stdout)
	}
}

func TestAgentREPL_Write_Read_Roundtrip(t *testing.T) {
	t.Parallel()
	be, cleanup := newREPLBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	path := filepath.Join(dir, "rt.bin")
	want := make([]byte, 4096)
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
		t.Errorf("contents differ")
	}
}

func TestAgentREPL_NotFoundErrorPropagation(t *testing.T) {
	t.Parallel()
	be, cleanup := newREPLBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := be.Read(ctx, "/no/such/file/anywhere")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err=%v, want ErrNotExist", err)
	}
}

func TestAgentREPL_StderrSeparated(t *testing.T) {
	t.Parallel()
	be, cleanup := newREPLBackend(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := be.Run(ctx, "echo o; echo e 1>&2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), "o") {
		t.Errorf("stdout=%q", res.Stdout)
	}
	if !strings.Contains(string(res.Stderr), "e") {
		t.Errorf("stderr=%q", res.Stderr)
	}
	if strings.Contains(string(res.Stdout), "e") {
		t.Errorf("stderr leaked into stdout: %q", res.Stdout)
	}
}

// TestAgentREPL_VsOneShot_Latency is a smoke check — it doesn't
// enforce a strict ratio, but logs both for visual confirmation when
// running with -v that REPL is faster.
func TestAgentREPL_VsOneShot_Latency(t *testing.T) {
	t.Parallel()
	const iterations = 5

	// REPL
	replBE, replCleanup := newREPLBackend(t)
	defer replCleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := replBE.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	startREPL := time.Now()
	for i := 0; i < iterations; i++ {
		if err := replBE.Probe(ctx); err != nil {
			t.Fatalf("REPL Probe[%d]: %v", i, err)
		}
	}
	replDur := time.Since(startREPL)

	// One-shot
	oneBE, oneCleanup := newAgentBackend(t)
	defer oneCleanup()
	startOne := time.Now()
	for i := 0; i < iterations; i++ {
		if err := oneBE.Probe(ctx); err != nil {
			t.Fatalf("OneShot Probe[%d]: %v", i, err)
		}
	}
	oneDur := time.Since(startOne)

	t.Logf("%d×Probe — REPL=%s OneShot=%s (REPL is %.1fx faster)",
		iterations, replDur, oneDur, float64(oneDur)/float64(replDur))

	// Loose sanity check: REPL should not be slower than one-shot
	// by more than a small margin (the test confirms the headline
	// claim is at least directionally true).
	if replDur > oneDur {
		t.Errorf("REPL was slower than one-shot: %s vs %s", replDur, oneDur)
	}
}
