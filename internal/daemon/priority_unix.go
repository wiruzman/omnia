//go:build darwin || linux || freebsd || netbsd || openbsd

package daemon

import "syscall"

func lowerDaemonPriority() error {
	return syscall.Setpriority(syscall.PRIO_PROCESS, 0, 10)
}
