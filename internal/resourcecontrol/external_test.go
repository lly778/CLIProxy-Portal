package resourcecontrol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExternalHostMetricsDriveWholeMachineAdmissionAndHistory(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "esxi.json")
	cpus, hostUsed := 93.0, 14000.0
	data, err := json.Marshal(externalMetrics{Source: "esxi", At: now, CPUCount: 4, CPUPercent: &cpus, HostUsedMiB: &hostUsed, HostTotalMiB: 24420})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	used := 120.0
	m := Metrics{At: now, HostID: "cpa-suite", CPUCount: 4, CPUPercent: 2, TotalMiB: 11444, AvailableMiB: 9700, SuiteUsedMiB: &used}
	m, err = applyExternalMetrics(m, path, now)
	if err != nil || !m.Fresh(now) || m.CPUPercent != cpus || m.UsedMiB() != hostUsed || m.TotalMiB != 11444 {
		t.Fatal("external host metrics were not used consistently", m, err)
	}
	p := DefaultPolicy()
	p.Mode, p.MemoryMB = "enforce", 20000
	c := NewController(p)
	c.UpdateMetrics(m, now)
	lease, reason := c.Acquire("test")
	if lease == nil {
		t.Fatal("host memory incorrectly used for suite admission", reason)
	}
	lease.Finish()
	history := c.History(now)
	if len(history) != 1 || history[0].UsedMiB != hostUsed || history[0].TotalMiB != 24420 || history[0].CPUPercent != cpus {
		t.Fatal("host history mixed with suite statistics", history)
	}
	for _, seconds := range []int{8, 16} {
		m.At = now.Add(time.Duration(seconds) * time.Second)
		c.UpdateMetrics(m, m.At)
	}
	if c.paused != "cpu_pressure" {
		t.Fatal("host CPU not used for protection", c.paused)
	}
	hostUsed = 22000
	m.HostUsedMiB = &hostUsed
	m.At = now.Add(18 * time.Second)
	c.UpdateMetrics(m, m.At)
	if c.paused != "memory_pressure" {
		t.Fatal("external host memory protection lost", c.paused)
	}
}

func TestExternalMetricsRejectStaleInvalidOrMissingDataWithoutVMFallback(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "esxi.json")
	for _, raw := range []string{
		`{}`, `invalid`,
		`{"source":"esxi","generated_at":"2020-01-01T00:00:00Z","cpu_count":4,"cpu_percent":12,"host_used_mib":14000,"host_total_mib":24420}`,
		`{"source":"esxi","generated_at":"` + now.UTC().Format(time.RFC3339Nano) + `","cpu_count":4,"cpu_percent":12,"host_used_mib":25000,"host_total_mib":24420}`,
		`{"source":"esxi","generated_at":"` + now.Add(time.Minute).UTC().Format(time.RFC3339Nano) + `","cpu_count":4,"cpu_percent":12,"host_used_mib":14000,"host_total_mib":24420}`,
		`{"source":"esxi","generated_at":"` + now.UTC().Format(time.RFC3339Nano) + `","cpu_count":4,"host_used_mib":14000,"host_total_mib":24420}`,
	} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := applyExternalMetrics(goodMetrics(now), path, now); err == nil {
			t.Fatal("invalid external source silently accepted", raw)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := applyExternalMetrics(goodMetrics(now), path, now); err == nil {
		t.Fatal("missing external source silently fell back to VM")
	}
}
