//go:build linux

package backup

import (
	"errors"
	"syscall"
)

func requireSpace(path string, required int64) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return errors.New("无法确认灾备临时目录可用空间")
	}
	if stat.Bsize <= 0 || stat.Bavail < uint64(required)/uint64(stat.Bsize)+1 {
		return errors.New("磁盘空间不足，无法安全生成快照、验证及回退副本；未停止服务")
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
