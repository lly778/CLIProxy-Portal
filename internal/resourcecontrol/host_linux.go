//go:build linux

package resourcecontrol

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// /proc/stat aggregates all host CPUs. Guest time is already included in user
// and nice, so only the first eight counters participate in the total.
func readHostCPUTimes(path string) (total, idle uint64, count int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, err
	}
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			if len(f) < 9 {
				return 0, 0, 0, errors.New("CPU 数据无效")
			}
			for i := 1; i <= 8; i++ {
				n, e := strconv.ParseUint(f[i], 10, 64)
				if e != nil {
					return 0, 0, 0, e
				}
				total += n
				if i == 4 || i == 5 {
					idle += n
				}
			}
			found = true
		} else if strings.HasPrefix(f[0], "cpu") {
			if n, e := strconv.Atoi(strings.TrimPrefix(f[0], "cpu")); e == nil && n >= 0 {
				count++
			}
		}
	}
	if !found || count == 0 {
		return 0, 0, 0, errors.New("整机 CPU 数据缺失")
	}
	return total, idle, count, nil
}

func readMemoryTimes(path string) (float64, float64, error) {
	b, e := os.ReadFile(path)
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
