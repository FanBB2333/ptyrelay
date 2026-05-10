package session

import "bytes"

// StripANSI removes the ANSI / control sequences that PTY-attached
// programs commonly emit and that pollute captured output.
//
// Coverage:
//   - CSI: ESC '[' ... final-byte (final ∈ 0x40–0x7E)
//   - OSC: ESC ']' ... BEL or ESC '\\'
//   - Two-byte ESC sequences: ESC X where X ∈ 0x40–0x5F (excluding '[' and ']')
//   - Bare backspace runs (delete previous byte)
//   - Carriage returns immediately followed by a newline are reduced to
//     just the newline (CRLF → LF). Lone CRs are preserved — some
//     programs use them for in-place updates, and stripping them silently
//     would change content semantics.
//
// This function operates on raw bytes, not runes. It does not attempt to
// decode multi-byte sequences beyond the structural ESC handling.
func StripANSI(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		b := in[i]
		switch {
		case b == 0x1B && i+1 < len(in):
			next := in[i+1]
			switch next {
			case '[':
				// CSI: scan until a final byte in 0x40–0x7E.
				j := i + 2
				for j < len(in) {
					c := in[j]
					if c >= 0x40 && c <= 0x7E {
						break
					}
					j++
				}
				if j >= len(in) {
					// Truncated CSI; drop the rest.
					return out
				}
				i = j
			case ']':
				// OSC: scan until BEL (0x07) or ESC \\.
				j := i + 2
				for j < len(in) {
					c := in[j]
					if c == 0x07 {
						break
					}
					if c == 0x1B && j+1 < len(in) && in[j+1] == '\\' {
						j++
						break
					}
					j++
				}
				if j >= len(in) {
					return out
				}
				i = j
			default:
				// Two-byte sequence: ESC X.
				if next >= 0x40 && next <= 0x5F {
					i++
					continue
				}
				// Unknown ESC sequence; keep both bytes.
				out = append(out, b)
			}
		case b == 0x08: // backspace
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		case b == '\r' && i+1 < len(in) && in[i+1] == '\n':
			// CRLF → LF
			out = append(out, '\n')
			i++
		default:
			out = append(out, b)
		}
	}
	return out
}

// NormalizeLineEndings turns \r\n into \n. It is a cheaper, narrower
// alternative to StripANSI when ANSI is not a concern but PTY-style line
// endings still are.
func NormalizeLineEndings(in []byte) []byte {
	return bytes.ReplaceAll(in, []byte("\r\n"), []byte("\n"))
}
