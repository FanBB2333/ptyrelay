// Command ptyrelay-agent is the remote-side binary that ptyrelay's
// AgentBackend talks to. It runs in two modes:
//
//   - one-shot (default): reads one line-delimited JSON request from
//     stdin, writes one line-delimited JSON response to stdout, exits.
//     Driven by AgentBackend over RunFramed + here-doc.
//
//   - repl: reads length-prefixed JSON requests from stdin in a loop,
//     responds in kind, exits on the `bye` op or on EOF. Driven by
//     AgentBackend over Session.Pipe.
//
// The dispatch is the same in both modes — only the wire codec differs.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/FanBB2333/ptyrelay/pkg/proto"
)

func main() {
	mode := flag.String("mode", "one-shot", "one-shot | repl")
	showVersion := flag.Bool("version", false, "print agent version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(proto.AgentVersion)
		return
	}

	switch *mode {
	case "one-shot":
		exitOnErr(runOneShot(os.Stdin, os.Stdout))
	case "repl":
		exitOnErr(runREPL(os.Stdin, os.Stdout))
	default:
		fmt.Fprintf(os.Stderr, "ptyrelay-agent: unknown mode %q\n", *mode)
		os.Exit(2)
	}
}

func exitOnErr(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, io.EOF) {
		return
	}
	fmt.Fprintf(os.Stderr, "ptyrelay-agent: %v\n", err)
	os.Exit(1)
}

func runOneShot(in io.Reader, out io.Writer) error {
	var req proto.Request
	if err := proto.ReadOneShot(in, &req); err != nil {
		// Surface as a malformed-protocol response rather than crashing
		// — gives AgentBackend a structured reply to parse.
		resp := errorResponse("", proto.ErrKindBadProto, err)
		_ = proto.WriteOneShot(out, resp)
		return err
	}
	resp := dispatch(&req)
	return proto.WriteOneShot(out, resp)
}

func runREPL(in io.Reader, out io.Writer) error {
	// Line-delimited JSON over the PTY-friendly path. Length-prefixed
	// framing (proto.WriteFrame / ReadFrame) is reserved for
	// binary-safe transports (e.g. WebSocket) where 4-byte BE length
	// headers don't risk hitting Ctrl-C / EOT byte values that a PTY
	// line discipline would interpret.
	dec := json.NewDecoder(in)
	for {
		var req proto.Request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			resp := errorResponse("", proto.ErrKindBadProto, err)
			_ = proto.WriteOneShot(out, resp)
			return err
		}
		resp := dispatch(&req)
		if err := proto.WriteOneShot(out, resp); err != nil {
			return err
		}
		if req.Op == proto.OpBye {
			return nil
		}
	}
}
