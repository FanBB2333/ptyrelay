package session

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func feedAll(t *testing.T, p *sentinelParser, chunks ...[]byte) (output []byte, exitCode int, ok bool) {
	t.Helper()
	for _, c := range chunks {
		done, err := p.feed(c)
		if err != nil {
			t.Fatalf("feed returned error: %v", err)
		}
		if done {
			return p.output, p.exitCode, true
		}
	}
	return nil, 0, false
}

func TestSentinel_HappyPath(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("garbage\n__PR_BEG_ab12__\nhello world\n__PR_END_ab12__:0\n")
	out, ec, ok := feedAll(t, p, stream)
	if !ok {
		t.Fatal("expected done")
	}
	if string(out) != "hello world" {
		t.Errorf("output = %q, want %q", out, "hello world")
	}
	if ec != 0 {
		t.Errorf("exit code = %d, want 0", ec)
	}
}

func TestSentinel_NonZeroExitCode(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\noutput\n__PR_END_ab12__:42\n")
	_, ec, ok := feedAll(t, p, stream)
	if !ok {
		t.Fatal("expected done")
	}
	if ec != 42 {
		t.Errorf("exit code = %d, want 42", ec)
	}
}

func TestSentinel_BEGAcrossChunks(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	chunks := [][]byte{
		[]byte("garbage\n__PR_BE"),
		[]byte("G_ab12__\nhel"),
		[]byte("lo\n__PR_END_ab12__:0\n"),
	}
	out, ec, ok := feedAll(t, p, chunks...)
	if !ok {
		t.Fatal("expected done")
	}
	if string(out) != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
	if ec != 0 {
		t.Errorf("exit code = %d", ec)
	}
}

func TestSentinel_ENDAcrossChunks(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	chunks := [][]byte{
		[]byte("__PR_BEG_ab12__\nfoo bar\n__PR_END_a"),
		[]byte("b12__:7"),
		[]byte("\n"),
	}
	out, ec, ok := feedAll(t, p, chunks...)
	if !ok {
		t.Fatal("expected done")
	}
	if string(out) != "foo bar" {
		t.Errorf("output = %q", out)
	}
	if ec != 7 {
		t.Errorf("exit code = %d", ec)
	}
}

func TestSentinel_OneByteAtATime(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\nlinear\n__PR_END_ab12__:3\n")
	for i := 0; i < len(stream); i++ {
		done, err := p.feed(stream[i : i+1])
		if err != nil {
			t.Fatalf("feed[%d] err: %v", i, err)
		}
		if done {
			if i != len(stream)-1 {
				t.Errorf("done at byte %d, want last byte", i)
			}
			break
		}
	}
	if string(p.output) != "linear" {
		t.Errorf("output = %q", p.output)
	}
	if p.exitCode != 3 {
		t.Errorf("exit code = %d", p.exitCode)
	}
}

func TestSentinel_EmptyOutput(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\n__PR_END_ab12__:0\n")
	out, ec, ok := feedAll(t, p, stream)
	if !ok {
		t.Fatal("expected done")
	}
	if len(out) != 0 {
		t.Errorf("output = %q, want empty", out)
	}
	if ec != 0 {
		t.Errorf("exit code = %d", ec)
	}
}

func TestSentinel_BinaryOutput(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	bin := []byte{0x00, 0xFF, '\n', 0x01, 0xFE}
	var buf bytes.Buffer
	buf.WriteString("__PR_BEG_ab12__\n")
	buf.Write(bin)
	buf.WriteString("\n__PR_END_ab12__:0\n")
	out, _, ok := feedAll(t, p, buf.Bytes())
	if !ok {
		t.Fatal("expected done")
	}
	if !bytes.Equal(out, bin) {
		t.Errorf("output = %x, want %x", out, bin)
	}
}

func TestSentinel_NonceIsolation(t *testing.T) {
	t.Parallel()
	// A different nonce in the stream must not match.
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_zz99__\nfoo\n__PR_END_zz99__:0\n__PR_BEG_ab12__\nactual\n__PR_END_ab12__:5\n")
	out, ec, ok := feedAll(t, p, stream)
	if !ok {
		t.Fatal("expected done")
	}
	if string(out) != "actual" {
		t.Errorf("output = %q, want 'actual'", out)
	}
	if ec != 5 {
		t.Errorf("exit code = %d", ec)
	}
}

func TestSentinel_MalformedExitCode(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\nfoo\n__PR_END_ab12__:not-a-number\n")
	_, err := p.feed(stream)
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
}

func TestSentinel_MaxOutputExceeded(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 16)
	stream := []byte("__PR_BEG_ab12__\n" + strings.Repeat("X", 32) + "\n__PR_END_ab12__:0\n")
	_, err := p.feed(stream)
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("err = %v, want ErrProtocol", err)
	}
}

func TestSentinel_FeedAfterDoneIsNoop(t *testing.T) {
	t.Parallel()
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\nx\n__PR_END_ab12__:0\n")
	if _, _, ok := feedAll(t, p, stream); !ok {
		t.Fatal("expected done")
	}
	done, err := p.feed([]byte("more bytes after done"))
	if err != nil {
		t.Errorf("feed-after-done err = %v", err)
	}
	if !done {
		t.Errorf("done should remain true after additional feed")
	}
}

func TestSentinel_CRLFBeforeMarkers(t *testing.T) {
	t.Parallel()
	// Cooked-mode PTY emits \r\n. Our state machine only requires the
	// trailing \n for the BEG marker line; CR before it lands in
	// captured output and is trimmed at the END boundary.
	p := newSentinelParser("ab12", 0)
	stream := []byte("__PR_BEG_ab12__\r\nhello\r\n__PR_END_ab12__:0\r\n")
	out, ec, ok := feedAll(t, p, stream)
	if !ok {
		t.Fatal("expected done")
	}
	// The output captured between markers retains the inner CR but the
	// trailing CR/LF before the END line is trimmed.
	if string(out) != "hello" {
		t.Errorf("output = %q, want 'hello'", out)
	}
	if ec != 0 {
		t.Errorf("exit code = %d", ec)
	}
}
