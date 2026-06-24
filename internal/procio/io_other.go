//go:build !darwin

package procio

import "fmt"

func Current(pid int) (DiskIO, error) {
	return DiskIO{}, fmt.Errorf("process disk I/O is not supported on this platform for pid %d", pid)
}
