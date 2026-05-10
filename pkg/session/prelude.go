package session

import "strings"

// ShellKind identifies the remote shell flavor so we can choose a prelude
// that won't produce errors on the chosen shell.
type ShellKind int

const (
	// ShellUnknown is the zero value; New requires a real value.
	ShellUnknown ShellKind = iota

	// ShellBash targets bash (any reasonable version).
	ShellBash

	// ShellZsh targets zsh.
	ShellZsh

	// ShellDash targets dash / busybox ash / generic POSIX sh.
	// History-control syntax differs from bash/zsh.
	ShellDash

	// ShellSh is the most defensive option — only POSIX-baseline
	// commands. Use when the remote shell is unknown.
	ShellSh
)

// String implements fmt.Stringer.
func (s ShellKind) String() string {
	switch s {
	case ShellBash:
		return "bash"
	case ShellZsh:
		return "zsh"
	case ShellDash:
		return "dash"
	case ShellSh:
		return "sh"
	default:
		return "unknown"
	}
}

// Prelude returns the shell snippet that prepares a session for framed
// RPC: silence echo and CR translation, lock the locale to C, mute the
// prompt, and disable history (where the syntax is supported).
//
// The snippet is a single shell line; it is wrapped in framing by
// FramedSession just like any other command.
func Prelude(s ShellKind) string {
	// Common: stty options exist on all reasonable Unixes; the
	// `2>/dev/null` swallows "Inappropriate ioctl for device" if the
	// process has no controlling tty (common in CI).
	common := strings.Join([]string{
		`stty -echo -onlcr -icanon 2>/dev/null`,
		`export LC_ALL=C LANG=C`,
		`PS1=''`,
		`unset PROMPT_COMMAND 2>/dev/null`,
	}, "; ")

	switch s {
	case ShellBash, ShellZsh:
		// `set +o history` is a bash/zsh extension. HISTFILE assignment
		// is harmless in any POSIX shell.
		return common + `; set +o history 2>/dev/null; HISTFILE=/dev/null`
	case ShellDash, ShellSh:
		// dash has no history; HISTFILE is harmless to set.
		return common + `; HISTFILE=/dev/null`
	default:
		return common
	}
}
