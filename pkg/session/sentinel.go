package session

import (
	"bytes"
	"fmt"
	"strconv"
)

// sentinelParser is a streaming state machine that extracts a single
// framed command's output and exit code from a byte stream.
//
// The wire shape produced by [wrapCommand] is, in order:
//
//	... pre-noise (echo of our command, prompt, etc.) ...
//	\n __PR_BEG_<nonce> __ \n
//	<command output bytes — may include CR, ANSI, anything>
//	\n __PR_END_<nonce> __ :<exit-code>\n
//	... post-noise (next prompt) ...
//
// The parser tolerates the BEG marker arriving in the middle of an
// already-buffered chunk, the END marker straddling a chunk boundary, and
// the exit-code line arriving after the END marker.
//
// Echo of our wrapper command CANNOT collide with the actual markers
// because [wrapCommand] inserts the nonce via a shell variable
// (`$__PR_N`); echo therefore contains the literal `$__PR_N` while the
// runtime printf produces the substituted form. We always scan for the
// substituted form.
type sentinelParser struct {
	begMarker []byte // "__PR_BEG_<nonce>__"
	endMarker []byte // "__PR_END_<nonce>__:"

	state parseState
	buf   []byte // bytes pending interpretation in the current state

	output   []byte
	exitCode int

	maxOutput int // 0 = no limit; non-zero caps output bytes
}

type parseState int

const (
	stateBeforeBEG parseState = iota
	stateCapturing
	stateReadingExitCode
	stateDone
)

func newSentinelParser(nonce string, maxOutput int) *sentinelParser {
	return &sentinelParser{
		begMarker: []byte("__PR_BEG_" + nonce + "__"),
		endMarker: []byte("__PR_END_" + nonce + "__:"),
		state:     stateBeforeBEG,
		maxOutput: maxOutput,
	}
}

// feed appends chunk and advances the state machine. It returns done=true
// once the exit-code line has been parsed; subsequent calls are a no-op.
//
// Errors are non-recoverable — they always indicate a protocol violation
// (malformed exit code, output exceeded maxOutput, etc.).
func (p *sentinelParser) feed(chunk []byte) (done bool, err error) {
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

func (p *sentinelParser) advance() (progressed, done bool, err error) {
	switch p.state {
	case stateBeforeBEG:
		idx := bytes.Index(p.buf, p.begMarker)
		if idx < 0 {
			// Trim the head: keep only the tail that might form a
			// partial prefix of the marker. We keep len(begMarker)-1
			// bytes so a marker spanning the next chunk still resolves.
			keep := len(p.begMarker) - 1
			if keep < 0 {
				keep = 0
			}
			if len(p.buf) > keep {
				p.buf = append(p.buf[:0], p.buf[len(p.buf)-keep:]...)
			}
			return false, false, nil
		}
		// Skip past the marker, then past the next \n. The marker is
		// always followed by \n in the wire format.
		afterMarker := idx + len(p.begMarker)
		nl := bytes.IndexByte(p.buf[afterMarker:], '\n')
		if nl < 0 {
			// Marker found, newline pending. Trim head up to the
			// marker so we keep buffering the (short) tail.
			p.buf = append(p.buf[:0], p.buf[idx:]...)
			return false, false, nil
		}
		p.buf = append(p.buf[:0], p.buf[afterMarker+nl+1:]...)
		p.state = stateCapturing
		return true, false, nil

	case stateCapturing:
		// Cap the buffer so a runaway command can't blow up RAM. We
		// don't flush mid-capture — finding END requires one full
		// scan, and partial flushing complicates trimming the printf
		// "\n" that precedes the END marker.
		if p.maxOutput > 0 && len(p.buf) > p.maxOutput {
			return false, false, fmt.Errorf("%w: output exceeded %d bytes", ErrProtocol, p.maxOutput)
		}
		idx := bytes.Index(p.buf, p.endMarker)
		if idx < 0 {
			return false, false, nil
		}
		// Found END. Output spans [0:lineStart) where lineStart is
		// the byte right after the most recent newline before idx.
		lineStart := idx
		for lineStart > 0 && p.buf[lineStart-1] != '\n' {
			lineStart--
		}
		outChunk := p.buf[:lineStart]
		// Strip exactly one trailing newline (and optional preceding
		// CR): that newline came from our wrapper's `printf '\n...'`,
		// not from the user's command output.
		if n := len(outChunk); n > 0 && outChunk[n-1] == '\n' {
			outChunk = outChunk[:n-1]
			if m := len(outChunk); m > 0 && outChunk[m-1] == '\r' {
				outChunk = outChunk[:m-1]
			}
		}
		if err := p.appendOutput(outChunk); err != nil {
			return false, false, err
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

func (p *sentinelParser) appendOutput(b []byte) error {
	if p.maxOutput > 0 && len(p.output)+len(b) > p.maxOutput {
		return fmt.Errorf("%w: output exceeded %d bytes", ErrProtocol, p.maxOutput)
	}
	p.output = append(p.output, b...)
	return nil
}
