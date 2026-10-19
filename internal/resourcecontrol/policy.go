package resourcecontrol

import (
	"errors"
	"math"
	"time"
)

const MiB int64 = 1 << 20

type Policy struct {
	Revision   int64   `json:"revision"`
	Mode       string  `json:"mode"`
	Global     int     `json:"global"`
	PerUser    int     `json:"per_user"`
	BodyMiB    int64   `json:"body_mib"`
	CPUPercent float64 `json:"cpu_percent"`
	MemoryMiB  int64   `json:"memory_mib"`
}

func DefaultPolicy() Policy {
	return Policy{Revision: 1, Mode: "observe", Global: 2, PerUser: 1, BodyMiB: 10, CPUPercent: 90, MemoryMiB: 768}
}
func (p Policy) Validate() error {
	if p.Revision < 1 || (p.Mode != "observe" && p.Mode != "enforce") || p.Global < 0 || p.Global > 128 || p.PerUser < 0 || p.PerUser > 128 || (p.Global > 0 && p.PerUser > p.Global) || p.BodyMiB < 0 || p.BodyMiB > 128 || math.IsNaN(p.CPUPercent) || math.IsInf(p.CPUPercent, 0) || p.CPUPercent < 0 || p.CPUPercent > 100 || p.MemoryMiB < 0 || p.MemoryMiB > 1048576 {
		return errors.New("资源保护参数无效：请检查模式、并发、请求大小及总 CPU、内存上限")
	}
	return nil
}

type Metrics struct {
	At           time.Time `json:"at"`
	HostID       string    `json:"host_id"`
	CPUCount     int       `json:"cpu_count"`
	CPUPercent   float64   `json:"cpu_percent"`
	TotalMiB     float64   `json:"total_mib"`
	AvailableMiB float64   `json:"available_mib"`
	Error        string    `json:"error,omitempty"`
}

func (m Metrics) UsedMiB() float64 { return math.Max(0, m.TotalMiB-m.AvailableMiB) }
func (m Metrics) Fresh(now time.Time) bool {
	return m.Error == "" && m.CPUCount > 0 && m.TotalMiB > 0 && m.AvailableMiB >= 0 && m.AvailableMiB <= m.TotalMiB && m.CPUPercent >= 0 && m.CPUPercent <= 100 && m.HostID != "" && !m.At.IsZero() && now.Sub(m.At) < 10*time.Second && now.Sub(m.At) > -5*time.Second
}
