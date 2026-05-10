package session

import (
	"bytes"
	"testing"
)

func TestStripANSI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"crlf-to-lf", "hello\r\nworld", "hello\nworld"},
		{"lone-cr-preserved", "hello\rworld", "hello\rworld"},
		{"backspace", "abc\b\bx", "ax"},
		{"backspace-on-empty", "\b\babc", "abc"},
		{"csi-color", "\x1b[31mred\x1b[0m", "red"},
		{"csi-cursor", "abc\x1b[2Adef", "abcdef"},
		{"csi-multi", "\x1b[1;31;42mboldredgreen\x1b[m end", "boldredgreen end"},
		{"osc-bel-terminated", "\x1b]0;title here\x07after", "after"},
		{"osc-st-terminated", "\x1b]0;title\x1b\\after", "after"},
		{"two-byte-esc", "before\x1bMafter", "beforeafter"},
		{"truncated-csi", "abc\x1b[", "abc"},
		{"truncated-osc", "abc\x1b]title", "abc"},
		{"binary-untouched", "\x00\x01\x02", "\x00\x01\x02"},
		{"mixed", "\x1b[31m\x1b]0;t\x07hello\b\b\bworld\r\n", "heworld\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripANSI([]byte(tc.in))
			if !bytes.Equal(got, []byte(tc.want)) {
				t.Errorf("StripANSI(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeLineEndings(t *testing.T) {
	t.Parallel()
	got := NormalizeLineEndings([]byte("a\r\nb\r\nc"))
	if string(got) != "a\nb\nc" {
		t.Errorf("got %q", got)
	}
}
