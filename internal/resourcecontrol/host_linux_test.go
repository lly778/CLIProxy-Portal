//go:build linux

package resourcecontrol

import (
	"testing"
	"time"
)

func TestHostMetricsReadActualProcCounters(t *testing.T) {
	var total, idle uint64
	first, e := collectMetrics(&total, &idle)
	if e == nil || first.Error != "" || total == 0 {
		t.Fatal("first CPU sample must warm up", e)
	}
	time.Sleep(20 * time.Millisecond)
	m, e := collectMetrics(&total, &idle)
	if e != nil || !m.Fresh(time.Now()) || m.UsedMiB() < 0 || m.TotalMiB <= 0 {
		t.Fatal("cannot read proc counters", m, e)
	}
}

func BenchmarkHostSampling(b *testing.B) {
	var total, idle uint64
	_, _ = collectMetrics(&total, &idle)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := collectMetrics(&total, &idle); e != nil {
			b.Fatal(e)
		}
	}
}
