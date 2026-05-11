package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/internal/testpty"
	"github.com/FanBB2333/ptyrelay/pkg/backend/agent"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

// buildAgentB is a B-flavored copy of buildAgent. We duplicate instead
// of refactoring the helper because keeping benchmark setup separate
// from test setup avoids spreading testing.TB through call sites that
// don't otherwise need it.
func buildAgentB(b *testing.B) string {
	b.Helper()
	out := filepath.Join(b.TempDir(), "ptyrelay-agent")
	cmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", out, "./cmd/ptyrelay-agent")
	cmd.Dir = repoRootB(b)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("go build agent: %v\n%s", err, buildOut)
	}
	return out
}

func repoRootB(b *testing.B) string {
	b.Helper()
	cwd, _ := os.Getwd()
	for d := cwd; d != "/" && d != ""; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
	}
	b.Fatal("could not locate go.mod")
	return ""
}

// BenchmarkAgent_Probe_OneShot measures the cost of one ping op in
// one-shot mode: a fresh agent process per call, with the request body
// staged on the remote via base64-chunked printf appends.
//
// Expected scale: ~300 ms / op on macOS, dominated by bash + agent fork
// and the chunked-write round-trips.
func BenchmarkAgent_Probe_OneShot(b *testing.B) {
	agentPath := buildAgentB(b)
	ch := testpty.NewBashB(b)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	be := agent.New(sess, agentPath) // default = ModeOneShot

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := be.Probe(ctx); err != nil {
			b.Fatalf("Probe: %v", err)
		}
	}
}

// BenchmarkAgent_Probe_REPL measures the same op against a long-lived
// agent process: one fork at startup, then every op is a JSON
// round-trip over Session.Pipe.
//
// Expected scale: sub-millisecond / op once warmed up — the speedup
// over one-shot is the entire point of REPL mode.
func BenchmarkAgent_Probe_REPL(b *testing.B) {
	agentPath := buildAgentB(b)
	ch := testpty.NewBashB(b)
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	be := agent.New(sess, agentPath, agent.WithMode(agent.ModeREPL))
	defer be.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// One warmup so b.N starts at steady state. The first call pays
	// the agent fork + warmup-ping; subsequent calls don't.
	if err := be.Probe(ctx); err != nil {
		b.Fatalf("warmup Probe: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := be.Probe(ctx); err != nil {
			b.Fatalf("Probe[%d]: %v", i, err)
		}
	}
}
