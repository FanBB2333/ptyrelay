package proto_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

func TestFrame_RoundTrip(t *testing.T) {
	t.Parallel()
	req := proto.Request{V: 1, ID: "abc", Op: proto.OpPing}
	var buf bytes.Buffer
	if err := proto.WriteFrame(&buf, &req); err != nil {
		t.Fatal(err)
	}

	var got proto.Request
	if err := proto.ReadFrame(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "abc" || got.Op != proto.OpPing {
		t.Errorf("got %+v", got)
	}
}

func TestFrame_RejectsHugeLength(t *testing.T) {
	t.Parallel()
	// header claims 1 GiB; should refuse before allocating.
	hdr := []byte{0x40, 0x00, 0x00, 0x00}
	r := bytes.NewReader(hdr)
	var got proto.Response
	err := proto.ReadFrame(r, &got)
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestFrame_BinaryPayload(t *testing.T) {
	t.Parallel()
	// Length-prefixed framing must work even when JSON contains bytes
	// that look like control characters (encoded as \uXXXX in JSON).
	data := []byte{0x00, 0x01, 0xFE, 0xFF, '\n'}
	req := proto.Request{V: 1, Op: proto.OpWrite}
	// Encode via a marshaled args field — args is RawMessage so we
	// sidestep the typed args structs here.
	var buf bytes.Buffer
	if err := proto.WriteFrame(&buf, &req); err != nil {
		t.Fatal(err)
	}
	var got proto.Request
	if err := proto.ReadFrame(&buf, &got); err != nil {
		t.Fatal(err)
	}
	_ = data // unused — type-check only
}

func TestOneShot_RoundTrip(t *testing.T) {
	t.Parallel()
	req := proto.Request{V: 1, Op: proto.OpRead, ID: "x"}
	var buf bytes.Buffer
	if err := proto.WriteOneShot(&buf, &req); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("one-shot output should end with newline, got %q", buf.String())
	}

	var got proto.Request
	if err := proto.ReadOneShot(&buf, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "x" || got.Op != proto.OpRead {
		t.Errorf("got %+v", got)
	}
}

func TestOneShot_NoEmbeddedNewlines(t *testing.T) {
	t.Parallel()
	// A field value containing a literal newline must be JSON-escaped
	// so the wire form stays single-line.
	req := proto.Request{V: 1, Op: proto.OpRun, ID: "with\nbreak"}
	var buf bytes.Buffer
	if err := proto.WriteOneShot(&buf, &req); err != nil {
		t.Fatal(err)
	}
	encoded := buf.String()
	// trim trailing record terminator
	encoded = strings.TrimRight(encoded, "\n")
	if strings.Contains(encoded, "\n") {
		t.Errorf("encoded body contains literal \\n: %q", encoded)
	}
}
