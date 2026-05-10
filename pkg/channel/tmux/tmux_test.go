package tmux_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/channel/tmux"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// newTmuxSession spins up an isolated tmux server (per-test socket) so
// parallel tests don't trip over each other and so `tmux kill-server`
// teardown is contained to this test.
func newTmuxSession(t *testing.T) (tmux.Options, func()) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH")
	}

	// Unix domain socket paths are capped at ~104 chars on macOS and
	// ~108 on Linux. t.TempDir() embeds the test name and is often
	// too long; mkdtemp under the OS temp root keeps it short.
	dir, err := os.MkdirTemp("", "ptr-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	init := tmux.InitOptions{
		Socket:       socket,
		SocketIsPath: true,
		SessionName:  "ptyrelay-test",
		Command:      bash + " --noprofile --norc -i",
		Width:        120,
		Height:       40,
	}
	chOpts, err := tmux.InitSession(ctx, init)
	if err != nil {
		t.Fatalf("InitSession: %v", err)
	}

	cleanup := func() {
		_ = tmux.KillSession(context.Background(), init)
	}
	return chOpts, cleanup
}

func TestTmuxChannel_BasicFraming(t *testing.T) {
	t.Parallel()
	opts, cleanup := newTmuxSession(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := tmux.New(ctx, opts)
	if err != nil {
		t.Fatalf("tmux.New: %v", err)
	}
	defer ch.Close()

	sess := session.New(ch, session.ShellBash)
	defer sess.Close()

	res, err := sess.RunFramed(ctx, "echo from-tmux", nil)
	if err != nil {
		t.Fatalf("RunFramed: %v", err)
	}
	if !strings.Contains(string(res.Output), "from-tmux") {
		t.Errorf("output = %q, want to contain 'from-tmux'", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
}

func TestTmuxChannel_BackendReadWrite(t *testing.T) {
	t.Parallel()
	opts, cleanup := newTmuxSession(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch, err := tmux.New(ctx, opts)
	if err != nil {
		t.Fatalf("tmux.New: %v", err)
	}
	defer ch.Close()

	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	be := shell.New(sess)

	dir := t.TempDir()
	path := filepath.Join(dir, "tmux-roundtrip.txt")
	want := []byte("data via tmux\n")

	if err := be.Write(ctx, path, want, 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := be.Read(ctx, path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTmuxChannel_PaneNotFound(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found")
	}

	dir, err := os.MkdirTemp("", "ptr-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, newErr := tmux.New(ctx, tmux.Options{
		Pane:         "%999",
		Socket:       socket,
		SocketIsPath: true,
	})
	if newErr == nil {
		t.Fatal("expected error for nonexistent pane")
	}
}
