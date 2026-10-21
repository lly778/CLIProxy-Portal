//go:build linux

package resourcecontrol

import "testing"

func TestHostMemoryCapacityReadsActualProcCounters(t *testing.T) {
	total, available, err := readMemoryTimes("/proc/meminfo")
	if err != nil || total <= 0 || available < 0 || available > total {
		t.Fatal(total, available, err)
	}
}
