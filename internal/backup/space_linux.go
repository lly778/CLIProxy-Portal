//go:build linux

package backup

import (
	"errors"
	"fmt"
	"syscall"
)

func requireSpace(path string, required int64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return errors.New("无法确认灾备临时目录可用空间")
	}
	if stat.Bsize <= 0 || required < 0 {
		return errors.New("无法确认灾备临时目录可用空间")
	}
	available := stat.Bavail * uint64(stat.Bsize)
	if available < uint64(required) {
		return fmt.Errorf("磁盘空间不足：至少需要 %.1f MiB 可用，当前仅 %.1f MiB；本次操作已中止", float64(required)/(1<<20), float64(available)/(1<<20))
	}
	return nil
}

func managerLease(file string) (func(), error) {
	f, err := openManagerLease(file)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("CPAMP 快照仍在执行，不能并发启动服务")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
