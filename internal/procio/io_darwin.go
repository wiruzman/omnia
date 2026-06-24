//go:build darwin

package procio

/*
#include <libproc.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func Current(pid int) (DiskIO, error) {
	var info C.struct_rusage_info_v4
	if rc, err := C.proc_pid_rusage(C.int(pid), C.RUSAGE_INFO_V4, (*C.rusage_info_t)(unsafe.Pointer(&info))); rc != 0 {
		return DiskIO{}, fmt.Errorf("proc_pid_rusage(%d): %v", pid, err)
	}
	return DiskIO{
		BytesRead:           uint64(info.ri_diskio_bytesread),
		BytesWritten:        uint64(info.ri_diskio_byteswritten),
		LogicalBytesWritten: uint64(info.ri_logical_writes),
	}, nil
}
