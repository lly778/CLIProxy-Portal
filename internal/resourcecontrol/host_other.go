//go:build !linux

package resourcecontrol

import (
	"errors"
	"time"
)

func newHostSampler() func() (Metrics, error) {
	return func() (Metrics, error) {
		return Metrics{At: time.Now().UTC()}, errors.New("整机资源监测需要 Linux proc")
	}
}
