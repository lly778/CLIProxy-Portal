//go:build linux

package resourcecontrol

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// proc counters report host totals in a standard Docker deployment. Each
// sampler owns its CPU baseline; no subprocess, Docker API or disk cache is used.
func newHostSampler() func() (Metrics, error) {
	var total, idle uint64
	return func() (Metrics, error) { return collectMetrics(&total, &idle) }
}
func cpuTimes() (uint64, uint64, error) {
	b, e := os.ReadFile("/proc/stat")
	if e != nil {
		return 0, 0, e
	}
	fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
	if len(fields) < 9 {
		return 0, 0, errors.New("CPU 数据无效")
	}
	var total, idle uint64
	for i := 1; i <= 8; i++ {
		n, e := strconv.ParseUint(fields[i], 10, 64)
		if e != nil {
			return 0, 0, e
		}
		total += n
		if i == 4 || i == 5 {
			idle += n
		}
	}
	return total, idle, nil
}
func memoryTimes() (float64, float64, error) {
	b, e := os.ReadFile("/proc/meminfo")
	if e != nil {
		return 0, 0, e
	}
	var total, avail float64
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.ParseFloat(f[1], 64)
		if f[0] == "MemTotal:" {
			total = n / 1024
		}
		if f[0] == "MemAvailable:" {
			avail = n / 1024
			found = true
		}
	}
	if total <= 0 || !found || avail < 0 || avail > total {
		return 0, 0, errors.New("内存数据无效")
	}
	return total, avail, nil
}

// Read proc counters only. This process does not call Docker or change limits.
func collectMetrics(lastTotal, lastIdle *uint64) (Metrics, error) {
	m := Metrics{At: time.Now().UTC(), HostID: "local-host", CPUCount: runtime.NumCPU()}
	total, idle, e := cpuTimes()
	if e != nil {
		return m, e
	}
	if *lastTotal > 0 && total > *lastTotal && idle >= *lastIdle {
		m.CPUPercent = 100 * (1 - float64(idle-*lastIdle)/float64(total-*lastTotal))
	} else if *lastTotal == 0 {
		e = errors.New("等待第二次 CPU 采样")
	}
	*lastTotal, *lastIdle = total, idle
	var memoryErr error
	m.TotalMiB, m.AvailableMiB, memoryErr = memoryTimes()
	if memoryErr != nil {
		return m, memoryErr
	}
	return m, e
}
