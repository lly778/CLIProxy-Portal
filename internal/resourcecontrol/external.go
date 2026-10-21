package resourcecontrol

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"time"
)

// ESXi credentials remain in the host collector; the portal reads only metrics.
type externalMetrics struct {
	Source       string    `json:"source"`
	At           time.Time `json:"generated_at"`
	CPUCount     int       `json:"cpu_count"`
	CPUPercent   *float64  `json:"cpu_percent"`
	HostUsedMiB  *float64  `json:"host_used_mib"`
	HostTotalMiB float64   `json:"host_total_mib"`
}

func applyExternalMetrics(m Metrics, path string, now time.Time) (Metrics, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return m, errors.New("ESXi 宿主机监测数据不可用")
	}
	var v externalMetrics
	if len(b) > 16384 || json.Unmarshal(b, &v) != nil || v.Source != "esxi" ||
		v.At.IsZero() || now.Sub(v.At) >= 15*time.Second || now.Sub(v.At) < -5*time.Second ||
		v.CPUCount <= 0 || v.CPUPercent == nil || v.HostUsedMiB == nil ||
		*v.CPUPercent < 0 || *v.CPUPercent > 100 || math.IsNaN(*v.CPUPercent) || math.IsInf(*v.CPUPercent, 0) ||
		v.HostTotalMiB <= 0 || math.IsInf(v.HostTotalMiB, 0) || math.IsNaN(v.HostTotalMiB) ||
		*v.HostUsedMiB < 0 || *v.HostUsedMiB > v.HostTotalMiB || math.IsNaN(*v.HostUsedMiB) {
		return m, errors.New("ESXi 宿主机监测数据无效或已过期")
	}
	m.CPUPercent, m.CPUCount = *v.CPUPercent, v.CPUCount
	m.HostUsedMiB, m.HostTotalMiB = v.HostUsedMiB, v.HostTotalMiB
	return m, nil
}

// Configure once before Run. A configured source never falls back to VM CPU.
func (c *Controller) SetHostMetricsPath(path string) {
	c.hostMetricsPath = path
}
