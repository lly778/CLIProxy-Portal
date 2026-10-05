//go:build !linux

package backup

import "os"

func inheritOwner(dir string, file *os.File) error { return nil }
