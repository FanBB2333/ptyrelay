package session

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
)

// streamingParser is the same state machine as sentinelParser, but
// instead of buffering captured bytes for retrieval at the end, it
// flushes them to an [io.Writer] as they become safely emittable.
//
// "Safely emittable" means: bytes that cannot still form part of an
// incoming END marker. To guarantee that, the parser keeps a trailing
// window of (len(endMarker)-1) bytes in its internal buffer at all
// times during stateStreaming; once those bytes age out of the window
// they're flushed to the writer.
//
// One side-effect of streaming: in rare boundary cases — when the
// wrapper's leading newline (the `\n` that printf emits right before
// END) lands exactly at the end of a Feed call's chunk — that newline
// has already been flushed to the writer by the time END is detected,
// and we cannot retroactively unsend it. The user's output stream then
// carries one extra `\n` at the tail. Callers that depend on exact
// byte-for-byte parity with RunFramed should not use Pipe; the v0.2.0
// REPL agent — the one consumer that exists — parses line-delimited
// JSON and tolerates trailing whitespace fine.
type streamingParser struct {
	begMarker, endMarker []byte

	state parseState
	buf   []byte
	out   io.Writer

	exitCode int
}

func newStreamingParser(nonce string, out io.Writer) *streamingParser {
	return &streamingParser{
		begMarker: []byte("__PR_BEG_" + nonce + "__"),
		endMarker: []byte("__PR_END_" + nonce + "__:"),
		state:     stateBeforeBEG,
		out:       out,
	}
}

func (p *streamingParser) feed(chunk []byte) (done bool, err error) {
	if p.state == stateDone {
		return true, nil
	}
	p.buf = append(p.buf, chunk...)
	for {
		progressed, finished, ferr := p.advance()
		if ferr != nil {
			return false, ferr
		}
		if finished {
			return true, nil
		}
		if !progressed {
			return false, nil
		}
	}
}

func (p *streamingParser) advance() (progressed, done bool, err error) {
	switch p.state {
	case stateBeforeBEG:
		idx := bytes.Index(p.buf, p.begMarker)
		if idx < 0 {
			keep := len(p.begMarker) - 1
			if keep < 0 {
				keep = 0
			}
			if len(p.buf) > keep {
				p.buf = append(p.buf[:0], p.buf[len(p.buf)-keep:]...)
			}
			return false, false, nil
		}
		afterMarker := idx + len(p.begMarker)
		nl := bytes.IndexByte(p.buf[afterMarker:], '\n')
		if nl < 0 {
			p.buf = append(p.buf[:0], p.buf[idx:]...)
			return false, false, nil
		}
		p.buf = append(p.buf[:0], p.buf[afterMarker+nl+1:]...)
		p.state = stateStreaming
		return true, false, nil

	case stateStreaming:
		idx := bytes.Index(p.buf, p.endMarker)
		if idx < 0 {
			// Flush strategy: deliver up to and including the most
			// recent newline. This unblocks line-delimited consumers
			// (REPL agents, json.Decoder) the moment a complete
			// line lands — required so a remote that emits one
			// response and waits for a follow-up request can be
			// driven synchronously.
			//
			// Side-effect (documented at the type level): if the
			// wrapper's trailing `\n` lands at a chunk boundary
			// before END is detectable, it gets flushed too and
			// the user stream picks up one extra `\n`. Line-
			// delimited consumers tolerate this (whitespace is
			// ignored between JSON values); strict-byte consumers
			// should use RunFramed.
			//
			// Hard fallback: if buf grows past 64 KiB with no
			// newline anywhere, flush all but the last
			// (len(endMarker)-1) bytes. Defends against
			// pathological binary-without-newlines producers.
			const noNewlineHardLimit = 64 * 1024
			keep := len(p.endMarker) - 1

			flushEnd := -1
			if last := bytes.LastIndexByte(p.buf, '\n'); last >= 0 {
				flushEnd = last + 1
			} else if len(p.buf) > noNewlineHardLimit {
				flushEnd = len(p.buf) - keep
			}
			if flushEnd > 0 {
				if _, werr := p.out.Write(p.buf[:flushEnd]); werr != nil {
					return false, false, werr
				}
				p.buf = append(p.buf[:0], p.buf[flushEnd:]...)
			}
			return false, false, nil
		}
		// Found END. Bytes [0:idx] are user output; strip a single
		// trailing wrapper newline (and optional preceding CR) when
		// it's still in the buffer. See the package-level doc on
		// rare boundary cases where this strip becomes a no-op.
		outChunk := p.buf[:idx]
		if n := len(outChunk); n > 0 && outChunk[n-1] == '\n' {
			outChunk = outChunk[:n-1]
			if m := len(outChunk); m > 0 && outChunk[m-1] == '\r' {
				outChunk = outChunk[:m-1]
			}
		}
		if len(outChunk) > 0 {
			if _, werr := p.out.Write(outChunk); werr != nil {
				return false, false, werr
			}
		}
		p.buf = append(p.buf[:0], p.buf[idx+len(p.endMarker):]...)
		p.state = stateReadingExitCode
		return true, false, nil

	case stateReadingExitCode:
		nl := bytes.IndexByte(p.buf, '\n')
		if nl < 0 {
			return false, false, nil
		}
		ecBytes := bytes.TrimSpace(p.buf[:nl])
		ec, perr := strconv.Atoi(string(ecBytes))
		if perr != nil {
			return false, false, fmt.Errorf("%w: malformed exit code %q", ErrProtocol, ecBytes)
		}
		p.exitCode = ec
		p.state = stateDone
		p.buf = nil
		return true, true, nil
	}
	return false, false, nil
}
