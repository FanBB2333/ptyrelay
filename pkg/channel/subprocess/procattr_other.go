//go:build !unix

package subprocess

import "syscall"

func procAttr() *syscall.SysProcAttr { return nil }
