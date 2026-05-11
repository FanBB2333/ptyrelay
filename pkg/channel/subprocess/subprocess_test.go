package subprocess_test

import (
	"context"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/FanBB2333/ptyrelay/pkg/backend/shell"
	"github.com/FanBB2333/ptyrelay/pkg/channel/subprocess"
	"github.com/FanBB2333/ptyrelay/pkg/session"
)

func TestSubprocess_EchoRoundTrip(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ch, err := subprocess.Start(ctx, subprocess.Options{
		Command: []string{"cat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	want := []byte{0, 1, 'a', 0xff, 0, 'b'}
	if _, err := ch.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(ch, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("roundtrip mismatch: got %v want %v", got, want)
	}
}

func TestSubprocess_EOFOnChildExit(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("true"); err != nil {
		t.Skip("true not found")
	}

	ctx := context.Background()
	ch, err := subprocess.Start(ctx, subprocess.Options{
		Command: []string{"true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// `true` writes nothing and exits 0; Read should see EOF.
	_, err = io.ReadAll(ch)
	if err != nil {
		t.Errorf("ReadAll: %v", err)
	}
}

func TestSubprocess_CloseIsIdempotent(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not found")
	}

	ch, err := subprocess.Start(context.Background(), subprocess.Options{
		Command: []string{"cat"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestSubprocess_MissingCommand(t *testing.T) {
	t.Parallel()
	_, err := subprocess.Start(context.Background(), subprocess.Options{
		Command: []string{"/nonexistent/binary-that-cannot-exist-12345"},
	})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

// TestSubprocess_SessionOverBash is the closure proof: same Backend
// works over a subprocess-backed Channel. This is the "docker /
// kubectl / lxc / podman path" without needing those tools installed —
// `bash` plays the same stdio role as `docker exec -i container bash`.
func TestSubprocess_SessionOverBash(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := subprocess.Start(ctx, subprocess.Options{
		Command: []string{"bash", "--noprofile", "--norc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess := session.New(ch, session.ShellBash)
	defer sess.Close()
	sb := shell.New(sess)

	if err := sb.Probe(ctx); err != nil {
		t.Fatalf("Probe over subprocess bash: %v", err)
	}
	res, err := sb.Run(ctx, "echo subprocess-marker", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(res.Stdout, "subprocess-marker") {
		t.Errorf("missing marker in stdout: %q", res.Stdout)
	}
}

func contains(b []byte, s string) bool {
	for i := 0; i+len(s) <= len(b); i++ {
		if string(b[i:i+len(s)]) == s {
			return true
		}
	}
	return false
}
