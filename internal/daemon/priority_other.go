//go:build !darwin && !linux && !freebsd && !netbsd && !openbsd

package daemon

func lowerDaemonPriority() error {
	return nil
}
