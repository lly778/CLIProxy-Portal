//go:build linux

package resourcecontrol

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSuiteMetricsUseHostCPUAndOnlyParentCgroupMemory(t *testing.T) {
	root, proc := t.TempDir(), t.TempDir()
	write := func(dir, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCPU := func(counters string) {
		t.Helper()
		write(proc, "stat", "cpu "+counters+"\ncpu0 0\ncpu1 0\ncpu2 0\ncpu3 0\n")
	}
	writeCPU("100 0 0 300 0 0 0 0 50 0")
	// Unrelated host memory must not count toward the suite memory limit.
	write(proc, "meminfo", "MemTotal: 8388608 kB\nMemAvailable: 1048576 kB\n")
	write(root, "memory.current", "419430400\n")
	write(root, "memory.stat", "anon 1\ninactive_file 104857600\n")
	// No cgroup CPU file exists: CPU must come entirely from the host.
	var total, idle uint64
	var at time.Time
	now := time.Now().Add(-6 * time.Second)
	if _, err := collectSuiteMetrics(root, &total, &idle, &at, now, proc); err == nil {
		t.Fatal("first CPU sample must warm up")
	}
	// Guest is included in user; counting it again would inflate utilization.
	writeCPU("200 0 0 600 0 0 0 0 150 0")
	m, err := collectSuiteMetrics(root, &total, &idle, &at, now.Add(2*time.Second), proc)
	if err != nil || !m.Fresh(m.At) || m.CPUPercent != 25 || m.UsedMiB() != 300 || m.AvailableMiB != 1024 {
		t.Fatalf("incorrect suite counters: %+v, %v", m, err)
	}
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.MemoryMiB = 500
	c := NewController(p)
	c.UpdateMetrics(m, m.At)
	lease, reason := c.Acquire("test")
	if lease == nil {
		t.Fatal("host memory was incorrectly included in suite limit", reason)
	}
	lease.Finish()
	p.CPUPercent = 20
	c = NewController(p)
	for _, offset := range []time.Duration{-16 * time.Second, -8 * time.Second, 0} {
		m.At = time.Now().Add(offset)
		c.UpdateMetrics(m, m.At)
	}
	if lease, reason := c.Acquire("test"); lease != nil || reason != "cpu_pressure" {
		t.Fatal("host CPU did not trigger protection", reason)
	}
	writeCPU("1 0 0 1 0 0 0 0")
	if _, err := collectSuiteMetrics(root, &total, &idle, &at, now.Add(4*time.Second), proc); err == nil {
		t.Fatal("reset CPU counters produced an accepted sample")
	}
	writeCPU("201 0 0 201 0 0 0 0")
	write(root, "memory.stat", "inactive_file 900000000\n")
	m, err = collectSuiteMetrics(root, &total, &idle, &at, now.Add(6*time.Second), proc)
	if err != nil || m.CPUPercent != 50 || m.UsedMiB() != 0 {
		t.Fatal("counter reset recovery or cache clamp failed", m, err)
	}
	if err := os.Remove(filepath.Join(root, "memory.stat")); err != nil {
		t.Fatal(err)
	}
	writeCPU("401 0 0 401 0 0 0 0")
	if _, err := collectSuiteMetrics(root, &total, &idle, &at, now.Add(8*time.Second), proc); err == nil {
		t.Fatal("missing suite memory fell back to host memory")
	}
}

func TestSuiteMemoryValidationAndAdmission(t *testing.T) {
	for _, used := range []float64{-1, math.NaN(), math.Inf(1), 2000} {
		m := goodMetrics(time.Now())
		m.HostID = "cpa-suite"
		m.SuiteUsedMiB = &used
		if m.Fresh(time.Now()) {
			t.Fatal("invalid suite memory accepted", used)
		}
	}
	m := goodMetrics(time.Now())
	m.HostID = "cpa-suite"
	if m.Fresh(time.Now()) {
		t.Fatal("suite sample without measured memory accepted")
	}
	used := 800.0
	m.SuiteUsedMiB = &used
	p := DefaultPolicy()
	p.Mode = "enforce"
	p.MemoryMiB = 768
	c := NewController(p)
	c.UpdateMetrics(m, m.At)
	if lease, reason := c.Acquire("test"); lease != nil || reason != "memory_pressure" {
		t.Fatal("suite memory limit not enforced", reason)
	}
	stale := m
	stale.At = time.Now().Add(-11 * time.Second)
	c = NewController(p)
	c.UpdateMetrics(stale, time.Now())
	if lease, reason := c.Acquire("test"); lease != nil || reason != "monitor_unavailable" {
		t.Fatal("stale monitoring did not fail closed", reason)
	}
}
