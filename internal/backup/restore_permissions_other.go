//go:build !linux

package backup

import (
	"errors"
	"os"
)

func trustedHostDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return errors.New("host directory invalid")
	}
	return nil
}
func trustedHostFile(file string) error {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("host file invalid")
	}
	return nil
}
func syncDirectory(dir string) error { return nil }

func trustedDeploymentFile(file string) error { return trustedHostFile(file) }
