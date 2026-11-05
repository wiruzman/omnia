//go:build darwin

package procio

import (
	"math"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestCurrentSampleCPUUnitsAndMemory(t *testing.T) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	sample, err := CurrentSample(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	userSeconds := float64(usage.Utime.Sec) + float64(usage.Utime.Usec)/1e6
	systemSeconds := float64(usage.Stime.Sec) + float64(usage.Stime.Usec)/1e6
	if math.Abs(sample.CPUUserSeconds-userSeconds) > 0.05 || math.Abs(sample.CPUSystemSeconds-systemSeconds) > 0.05 {
		t.Fatalf("CPU units do not match getrusage: %+v %+v", sample, usage)
	}
	if sample.ResidentBytes == 0 || sample.FootprintBytes == 0 {
		t.Fatalf("missing memory: %+v", sample)
	}
	if _, err := CurrentSample(-1); err == nil {
		t.Fatal("invalid pid accepted")
	}
}

func TestCPUTimeUnderLoadMatchesGetrusage(t *testing.T) {
	cpu := func() float64 {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	}
	before, err := CurrentSample(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	beforeCPU := cpu()
	deadline := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
	afterCPU := cpu()
	after, err := CurrentSample(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	got := after.CPUUserSeconds + after.CPUSystemSeconds - before.CPUUserSeconds - before.CPUSystemSeconds
	want := afterCPU - beforeCPU
	if math.Abs(got-want) > max(0.005, want*0.1) {
		t.Fatalf("CPU seconds under load: got %.6f, getrusage %.6f", got, want)
	}
}
