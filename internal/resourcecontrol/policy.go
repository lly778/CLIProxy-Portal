package resourcecontrol

import (
	"errors"
	"math"
	"time"
)

const MiB int64 = 1 << 20

const DefaultCgroupPath = "/run/cliproxy-suite"
const DefaultCgroupParent = "cliproxy.slice"

type Policy struct {
	Revision   int64   `json:"revision"`
	Mode       string  `json:"mode"`
	Global     int     `json:"global"`
	PerUser    int     `json:"per_user"`
	BodyMB     float64 `json:"body_mb"`
	CPUPercent float64 `json:"cpu_percent"`
	MemoryMB   float64 `json:"memory_mb"`
}

func DefaultPolicy() Policy {
	return Policy{Revision: 1, Mode: "observe", Global: 2, PerUser: 1, BodyMB: 10, CPUPercent: 90, MemoryMB: 768}
}
func (p Policy) Validate() error {
	if p.Revision < 1 || (p.Mode != "observe" && p.Mode != "enforce") || p.Global < 0 || p.Global > 128 || p.PerUser < 0 || p.PerUser > 128 || (p.Global > 0 && p.PerUser > p.Global) || math.IsNaN(p.BodyMB) || math.IsInf(p.BodyMB, 0) || p.BodyMB < 0 || p.BodyMB > 134.217728 || math.IsNaN(p.CPUPercent) || math.IsInf(p.CPUPercent, 0) || p.CPUPercent < 0 || p.CPUPercent > 100 || math.IsNaN(p.MemoryMB) || math.IsInf(p.MemoryMB, 0) || p.MemoryMB < 0 || p.MemoryMB > 1099511.627776 {
		return errors.New("资源保护参数无效：请检查模式、并发、请求大小及总 CPU、内存上限")
	}
	return nil
}

// BodyLimitBytes converts the decimal MB policy to an exact byte limit.
func (p Policy) BodyLimitBytes() int64 { return int64(math.Round(p.BodyMB * 1_000_000)) }

type Metrics struct {
	At           time.Time `json:"at"`
	HostID       string    `json:"host_id"`
	CPUCount     int       `json:"cpu_count"`
	CPUPercent   float64   `json:"cpu_percent"`
	TotalMiB     float64   `json:"total_mib"`
	AvailableMiB float64   `json:"available_mib"`
	SuiteUsedMiB *float64  `json:"suite_used_mib,omitempty"`
	HostUsedMiB  *float64  `json:"host_used_mib,omitempty"`
	HostTotalMiB float64   `json:"host_total_mib,omitempty"`
	Error        string    `json:"error,omitempty"`
}

func (m Metrics) UsedMiB() float64 {
	if m.HostUsedMiB != nil {
		return *m.HostUsedMiB
	}
	return math.Max(0, m.TotalMiB-m.AvailableMiB)
}
func (m Metrics) UsedMB() float64 { return m.UsedMiB() * float64(MiB) / 1_000_000 }
func (m Metrics) TotalMemoryMiB() float64 {
	if m.HostUsedMiB != nil {
		return m.HostTotalMiB
	}
	return m.TotalMiB
}
func (m Metrics) TotalMB() float64     { return m.TotalMemoryMiB() * float64(MiB) / 1_000_000 }
func (m Metrics) AvailableMB() float64 { return m.TotalMB() - m.UsedMB() }

func (m Metrics) Fresh(now time.Time) bool {
	used := m.UsedMiB()
	total := m.TotalMemoryMiB()
	return m.Error == "" && m.CPUCount > 0 && m.TotalMiB > 0 && !math.IsInf(m.TotalMiB, 0) && m.AvailableMiB >= 0 && m.AvailableMiB <= m.TotalMiB && total > 0 && !math.IsInf(total, 0) && used >= 0 && used <= total && m.CPUPercent >= 0 && m.CPUPercent <= 100 && m.HostID != "" && !m.At.IsZero() && now.Sub(m.At) < 10*time.Second && now.Sub(m.At) > -5*time.Second
}
