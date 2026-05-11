//go:build unix

package subprocess

import "syscall"

// procAttr puts the child in its own process group so SIGINT to the
// parent doesn't propagate, and Close() can target the group cleanly
// if it ever needs to (we currently only signal the leader).
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
