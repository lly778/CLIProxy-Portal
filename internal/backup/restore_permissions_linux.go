package backup

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func trustedHostDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("host directory permissions invalid")
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("host directory owner invalid")
	}
	return nil
}

func trustedHostFile(file string) error {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("host file permissions invalid")
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("host file owner invalid")
	}
	return trustedHostDirectory(filepath.Dir(file))
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func trustedDeploymentFile(file string) error {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("后台部署文件权限无效")
	}
	parent, err := os.Lstat(filepath.Dir(file))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0o022 != 0 || parent.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("后台部署目录权限无效")
	}
	return nil
}
