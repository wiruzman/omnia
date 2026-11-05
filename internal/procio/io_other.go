//go:build !darwin

package procio

import "fmt"

func Current(pid int) (DiskIO, error) {
	return DiskIO{}, fmt.Errorf("process disk I/O is not supported on this platform for pid %d", pid)
}

func CurrentSample(pid int) (Sample, error) {
	return Sample{}, fmt.Errorf("process measurements are not supported on this platform")
}
