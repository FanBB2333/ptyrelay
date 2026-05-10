// Package shellquote escapes strings for safe inclusion in POSIX shell
// command lines.
//
// The single-quote form (`'…'`) is the only POSIX-portable mechanism that
// suppresses every shell metacharacter; double quotes still expand `$`,
// `\“ and `\\`. Quote always emits single quotes.
package shellquote

import "strings"

// Quote returns s wrapped in POSIX-portable single quotes, with embedded
// single quotes encoded as the canonical `'\”` sequence.
//
// Examples:
//
//	Quote("hello")        → "'hello'"
//	Quote("don't")        → "'don'\\''t'"  (raw bytes: 'don'\''t')
//	Quote("$HOME; rm -rf /") → "'$HOME; rm -rf /'"
//
// The returned string is always one shell word.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// QuoteAll quotes each element of args independently and joins them with
// single spaces, suitable for splatting into a shell `$@`-style argument
// list.
func QuoteAll(args ...string) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = Quote(a)
	}
	return strings.Join(parts, " ")
}
