package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

// callOneShot runs runOneShot in-process — fast, no fork — for handler
// tests. Returns the response that came back over the wire.
func callOneShot(t *testing.T, req *proto.Request) *proto.Response {
	t.Helper()
	var inBuf bytes.Buffer
	if err := proto.WriteOneShot(&inBuf, req); err != nil {
		t.Fatal(err)
	}
	var outBuf bytes.Buffer
	if err := runOneShot(&inBuf, &outBuf); err != nil && err != io.EOF {
		t.Fatalf("runOneShot: %v", err)
	}
	var resp proto.Response
	if err := proto.ReadOneShot(&outBuf, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return &resp
}

func argsJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandlePing(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{V: 1, ID: "ping-1", Op: proto.OpPing})
	if !resp.OK {
		t.Fatalf("err=%q kind=%q", resp.Err, resp.ErrKind)
	}
	if resp.ID != "ping-1" {
		t.Errorf("id mismatch: %q", resp.ID)
	}
	var data proto.PingData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Version != 1 {
		t.Errorf("version=%d", data.Version)
	}
	if data.AgentVersion == "" {
		t.Error("AgentVersion empty")
	}
}

func TestHandleRead_File(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	want := []byte("hello agent")
	if err := os.WriteFile(path, want, 0o644); err != nil {
		t.Fatal(err)
	}

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRead,
		Args: argsJSON(t, proto.ReadArgs{Path: path}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.ReadData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data.Bytes, want) {
		t.Errorf("got %q, want %q", data.Bytes, want)
	}
}

func TestHandleRead_NotFound(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRead,
		Args: argsJSON(t, proto.ReadArgs{Path: "/no/such/file/here"}),
	})
	if resp.OK {
		t.Fatal("expected !ok")
	}
	if resp.ErrKind != proto.ErrKindNotFound {
		t.Errorf("kind=%q, want %q", resp.ErrKind, proto.ErrKindNotFound)
	}
}

func TestHandleWrite_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	want := []byte{0x00, 0x01, 0xFE, 0xFF, 'h', 'i', '\n'}

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpWrite,
		Args: argsJSON(t, proto.WriteArgs{Path: path, Bytes: want, Mode: 0o600}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("contents mismatch: got %x, want %x", got, want)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", st.Mode().Perm())
	}
}

func TestHandleStat_File(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpStat,
		Args: argsJSON(t, proto.StatArgs{Path: path}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.StatData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Size != 5 || data.IsDir || data.IsSymlink {
		t.Errorf("got %+v", data)
	}
}

func TestHandleLstat_Symlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpLstat,
		Args: argsJSON(t, proto.StatArgs{Path: link}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.StatData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !data.IsSymlink {
		t.Error("IsSymlink=false")
	}
	if data.SymlinkTarget != target {
		t.Errorf("target=%q, want %q", data.SymlinkTarget, target)
	}
}

func TestHandleList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644)
	}

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpList,
		Args: argsJSON(t, proto.ListArgs{Path: dir}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.ListData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Entries) != 3 {
		t.Errorf("entries=%d, want 3 (%v)", len(data.Entries), data.Entries)
	}
	for i, want := range []string{"a", "b", "c"} {
		if data.Entries[i].Name != want {
			t.Errorf("entry[%d]=%q, want %q", i, data.Entries[i].Name, want)
		}
	}
}

func TestHandleMkdirAll_RenameRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	deep := filepath.Join(dir, "x", "y", "z")

	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpMkdirAll,
		Args: argsJSON(t, proto.MkdirAllArgs{Path: deep, Mode: 0o750}),
	})
	if !resp.OK {
		t.Fatalf("MkdirAll err: %s", resp.Err)
	}
	if st, err := os.Stat(deep); err != nil || !st.IsDir() {
		t.Fatalf("dir not created")
	}

	src := filepath.Join(dir, "s")
	dst := filepath.Join(dir, "d")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	resp = callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRename,
		Args: argsJSON(t, proto.RenameArgs{OldPath: src, NewPath: dst}),
	})
	if !resp.OK {
		t.Fatalf("Rename err: %s", resp.Err)
	}
	resp = callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRemove,
		Args: argsJSON(t, proto.RemoveArgs{Path: dst}),
	})
	if !resp.OK {
		t.Fatalf("Remove err: %s", resp.Err)
	}
}

func TestHandleRun_StdoutStderrExit(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRun,
		Args: argsJSON(t, proto.RunArgs{
			Cmd: `echo out; echo err 1>&2; exit 7`,
		}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.RunData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data.Stdout), "out") {
		t.Errorf("stdout=%q", data.Stdout)
	}
	if !strings.Contains(string(data.Stderr), "err") {
		t.Errorf("stderr=%q", data.Stderr)
	}
	if data.ExitCode != 7 {
		t.Errorf("exit=%d", data.ExitCode)
	}
}

func TestHandleRun_Stdin(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{
		V: 1, Op: proto.OpRun,
		Args: argsJSON(t, proto.RunArgs{
			Cmd:   "tr a-z A-Z",
			Stdin: []byte("hello"),
		}),
	})
	if !resp.OK {
		t.Fatalf("err: %s", resp.Err)
	}
	var data proto.RunData
	_ = json.Unmarshal(resp.Data, &data)
	if !strings.Contains(string(data.Stdout), "HELLO") {
		t.Errorf("stdout=%q", data.Stdout)
	}
}

func TestUnknownOp(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{V: 1, Op: "no-such-op"})
	if resp.OK {
		t.Fatal("expected !ok")
	}
	if resp.ErrKind != proto.ErrKindUnknownOp {
		t.Errorf("kind=%q", resp.ErrKind)
	}
}

func TestVersionMismatch(t *testing.T) {
	t.Parallel()
	resp := callOneShot(t, &proto.Request{V: 999, Op: proto.OpPing})
	if resp.OK {
		t.Fatal("expected !ok")
	}
	if resp.ErrKind != proto.ErrKindBadProto {
		t.Errorf("kind=%q", resp.ErrKind)
	}
}

func TestREPL_PingThenBye(t *testing.T) {
	t.Parallel()
	var inBuf bytes.Buffer
	if err := proto.WriteOneShot(&inBuf, &proto.Request{V: 1, ID: "1", Op: proto.OpPing}); err != nil {
		t.Fatal(err)
	}
	if err := proto.WriteOneShot(&inBuf, &proto.Request{V: 1, ID: "2", Op: proto.OpBye}); err != nil {
		t.Fatal(err)
	}
	var outBuf bytes.Buffer
	if err := runREPL(&inBuf, &outBuf); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(&outBuf)
	var resp proto.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "1" {
		t.Errorf("first resp: %+v", resp)
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || resp.ID != "2" {
		t.Errorf("bye resp: %+v", resp)
	}
}
