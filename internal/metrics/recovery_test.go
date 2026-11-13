package metrics

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestRecoveryReasonsAreNumericCumulativeAndConcurrent(t *testing.T) {
	c := &Counters{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				for r := NativeMustScan; r <= DiscoveryOverflow; r++ {
					c.RecoveryEvent(r)
				}
				c.ScopedRecoveryRequested()
				c.GlobalRecoveryRequested(false)
				c.GlobalRecoveryRequested(true)
				c.GlobalRecoveryStarted()
			}
		}()
	}
	wg.Wait()
	w := c.take()
	for name, n := range map[string]uint64{"must_scan": w.NativeMustScan, "user_drop": w.NativeUserDropped, "kernel_drop": w.NativeKernelDropped, "ids": w.NativeIDsWrapped, "root": w.NativeRootChanged, "mount": w.NativeMount, "unmount": w.NativeUnmount, "delivery": w.DeliveryOverflows, "pending": w.PendingOverflows, "discovery": w.DiscoveryOverflows, "scoped": w.ScopedRecoveryRequests, "during": w.RecoveryDuringScan, "scans": w.GlobalRecoveryScans} {
		if n != 200 {
			t.Fatalf("%s=%d want 200", name, n)
		}
	}
	if w.GlobalRecoveryRequests != 400 || w.WatcherOverflows != 0 {
		t.Fatalf("reason observations conflated with coalesced work: %+v", w)
	}
	c.RecoveryEvent(RecoveryReason(255))
	if next := c.take(); next != w {
		t.Fatal("reason counters reset or unknown reason changed work")
	}
	data, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		if _, ok := value.(float64); !ok {
			t.Fatalf("non-numeric work field %s: %T", name, value)
		}
	}
	var disabled *Counters
	disabled.RecoveryEvent(NativeMustScan)
	disabled.ScopedRecoveryRequested()
	disabled.GlobalRecoveryRequested(true)
	disabled.GlobalRecoveryStarted()
}
