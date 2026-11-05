//go:build darwin

package procio

/*
#include <libproc.h>
#include <mach/mach_time.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

func Current(pid int) (DiskIO, error) {
	sample, err := CurrentSample(pid)
	return sample.DiskIO, err
}

func CurrentSample(pid int) (Sample, error) {
	var info C.struct_rusage_info_v4
	if rc, err := C.proc_pid_rusage(C.int(pid), C.RUSAGE_INFO_V4, (*C.rusage_info_t)(unsafe.Pointer(&info))); rc != 0 {
		return Sample{}, fmt.Errorf("proc_pid_rusage(%d): %v", pid, err)
	}
	// rusage_info CPU times use Mach absolute ticks, which are not nanoseconds
	// on Apple silicon. Convert through the platform timebase before persisting.
	var timebase C.mach_timebase_info_data_t
	if rc := C.mach_timebase_info(&timebase); rc != 0 || timebase.denom == 0 {
		return Sample{}, fmt.Errorf("read Mach timebase: %d", rc)
	}
	secondsPerTick := float64(timebase.numer) / float64(timebase.denom) / 1e9
	return Sample{DiskIO: DiskIO{
		BytesRead:           uint64(info.ri_diskio_bytesread),
		BytesWritten:        uint64(info.ri_diskio_byteswritten),
		LogicalBytesWritten: uint64(info.ri_logical_writes),
	}, CPUUserSeconds: float64(info.ri_user_time) * secondsPerTick,
		CPUSystemSeconds: float64(info.ri_system_time) * secondsPerTick,
		ResidentBytes:    uint64(info.ri_resident_size),
		FootprintBytes:   uint64(info.ri_phys_footprint)}, nil
}
