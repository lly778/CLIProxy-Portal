//go:build !linux

package resourcecontrol

import (
	"errors"
	"time"
)

func newSuiteSampler(_ string) func() (Metrics, error) {
	return func() (Metrics, error) {
		return Metrics{At: time.Now().UTC()}, errors.New("CPA 套件资源监测需要 Linux cgroup v2")
	}
}
