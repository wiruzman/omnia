package procio

type DiskIO struct {
	BytesRead           uint64 `json:"read_bytes"`
	BytesWritten        uint64 `json:"written_bytes"`
	LogicalBytesWritten uint64 `json:"logical_written_bytes"`
}

// Sample contains process-wide numeric measurements, never process arguments or paths.
type Sample struct {
	DiskIO
	CPUUserSeconds   float64
	CPUSystemSeconds float64
	ResidentBytes    uint64
	FootprintBytes   uint64
}
