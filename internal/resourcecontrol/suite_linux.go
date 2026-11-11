//go:build linux

package resourcecontrol

import (
	"errors"
	"path/filepath"
	"time"
)

// Both CPU and memory come from whole-machine proc counters. The root argument
// remains accepted for existing deployments; admission no longer reads cgroups.
func newSuiteSampler(root string) func() (Metrics, error) {
	var previousTotal, previousIdle uint64
	var at time.Time
	return func() (Metrics, error) {
		return collectSuiteMetrics(root, &previousTotal, &previousIdle, &at, time.Now(), "/proc")
	}
}

func collectSuiteMetrics(_ string, previousTotal, previousIdle *uint64, at *time.Time, now time.Time, proc string) (Metrics, error) {
	m := Metrics{At: now.UTC(), HostID: "host"}
	total, idle, count, err := readHostCPUTimes(filepath.Join(proc, "stat"))
	if err != nil {
		return m, err
	}
	m.CPUCount = count
	m.TotalMiB, m.AvailableMiB, err = readMemoryTimes(filepath.Join(proc, "meminfo"))
	if err != nil {
		return m, err
	}
	lastTotal, lastIdle, lastAt := *previousTotal, *previousIdle, *at
	*previousTotal, *previousIdle, *at = total, idle, now
	if lastAt.IsZero() || !now.After(lastAt) || total <= lastTotal || idle < lastIdle || idle-lastIdle > total-lastTotal {
		return m, errors.New("等待整机 CPU 有效采样")
	}
	m.CPUPercent = 100 * float64((total-lastTotal)-(idle-lastIdle)) / float64(total-lastTotal)
	if !m.Fresh(now) {
		return m, errors.New("整机资源采样无效")
	}
	return m, nil
}
