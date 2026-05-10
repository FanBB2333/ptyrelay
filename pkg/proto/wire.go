package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameSize bounds a single REPL message. 32 MiB is generous —
// agent op payloads are file content + base64 overhead, capped by the
// caller's own limits. Refusing larger frames protects against a
// malformed / hostile peer wedging us into giant allocations.
const MaxFrameSize = 32 << 20

// WriteFrame writes msg to w as a length-prefixed JSON frame:
//
//	[4 bytes big-endian length] [JSON body]
//
// Used by REPL transport; the prefix keeps message boundaries clean
// regardless of the JSON payload's content.
func WriteFrame(w io.Writer, msg any) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("proto: marshal: %w", err)
	}
	if len(body) > MaxFrameSize {
		return fmt.Errorf("proto: frame %d bytes exceeds %d", len(body), MaxFrameSize)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one length-prefixed JSON frame from r and unmarshals
// it into out (a pointer).
func ReadFrame(r io.Reader, out any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return fmt.Errorf("proto: frame length %d exceeds %d", n, MaxFrameSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("proto: read body: %w", err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("proto: unmarshal: %w", err)
	}
	return nil
}

// WriteOneShot writes msg as a single line of JSON terminated by \n.
//
// Used by one-shot transport (here-doc-delivered request, framed-response
// stdout). The line-delimited form is required because the surrounding
// shell sentinel framing operates on text streams.
func WriteOneShot(w io.Writer, msg any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(msg)
}

// ReadOneShot reads one line-delimited JSON message from r.
func ReadOneShot(r io.Reader, out any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		return fmt.Errorf("proto: decode: %w", err)
	}
	return nil
}
