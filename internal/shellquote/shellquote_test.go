package shellquote_test

import (
	"os/exec"
	"testing"

	"github.com/FanBB2333/ptyrelay/internal/shellquote"
)

func TestQuote(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"hello", "'hello'"},
		{"two words", "'two words'"},
		{"$HOME", "'$HOME'"},
		{"a;b|c&d>e<f", "'a;b|c&d>e<f'"},
		{"don't", `'don'\''t'`},
		{`back\slash`, `'back\slash'`},
		{"\n", "'\n'"},
		{`'`, `''\'''`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := shellquote.Quote(tc.in)
			if got != tc.want {
				t.Errorf("Quote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestQuote_RoundtripViaShell asks /bin/sh to read the quoted string back
// — the strongest evidence that the escape is correct, regardless of any
// shell-specific quirks.
func TestQuote_RoundtripViaShell(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	inputs := []string{
		"hello",
		"two words",
		"$HOME",
		"a;b|c&d>e<f",
		"don't",
		`back\slash`,
		"\n",
		`'`,
		`''`,
		`"double"`,
		`mixed 'quote" types`,
		"with\ttab",
		"path/with spaces/and 'apostrophes'",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			quoted := shellquote.Quote(in)
			// printf '%s' <quoted> writes the original bytes back to stdout.
			out, err := exec.Command("sh", "-c", "printf '%s' "+quoted).Output()
			if err != nil {
				t.Fatalf("shell run failed: %v", err)
			}
			if string(out) != in {
				t.Errorf("roundtrip: input=%q, shell saw=%q", in, out)
			}
		})
	}
}

func TestQuoteAll(t *testing.T) {
	t.Parallel()
	got := shellquote.QuoteAll("a", "b c", "d'e")
	want := `'a' 'b c' 'd'\''e'`
	if got != want {
		t.Errorf("QuoteAll = %q, want %q", got, want)
	}
}
