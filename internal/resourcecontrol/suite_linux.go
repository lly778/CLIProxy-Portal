//go:build linux

package resourcecontrol

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CPU comes from host-wide proc counters; memory comes from the read-only
// portal/CPA/CPAMP parent cgroup. Neither metric needs a Docker socket.
func newSuiteSampler(root string) func() (Metrics, error) {
	var previousTotal, previousIdle uint64
	var at time.Time
	return func() (Metrics, error) {
		return collectSuiteMetrics(root, &previousTotal, &previousIdle, &at, time.Now(), "/proc")
	}
}

func namedCounter(path, key string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			return strconv.ParseUint(f[1], 10, 64)
		}
	}
	return 0, errors.New("CPA 套件资源计数缺失")
}

func collectSuiteMetrics(root string, previousTotal, previousIdle *uint64, at *time.Time, now time.Time, proc string) (Metrics, error) {
	m := Metrics{At: now.UTC(), HostID: "cpa-suite"}
	total, idle, count, err := readHostCPUTimes(filepath.Join(proc, "stat"))
	if err != nil {
		return m, err
	}
	m.CPUCount = count
	current, err := os.ReadFile(filepath.Join(root, "memory.current"))
	if err != nil {
		return m, err
	}
	memory, err := strconv.ParseUint(strings.TrimSpace(string(current)), 10, 64)
	if err != nil {
		return m, err
	}
	inactive, err := namedCounter(filepath.Join(root, "memory.stat"), "inactive_file")
	if err != nil {
		return m, err
	}
	used := float64(memory) - math.Min(float64(memory), float64(inactive))
	used /= float64(MiB)
	m.SuiteUsedMiB = &used
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
		return m, errors.New("CPA 套件资源采样无效")
	}
	return m, nil
}
