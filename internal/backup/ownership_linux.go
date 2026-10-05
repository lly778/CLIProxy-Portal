package backup

import (
	"os"
	"syscall"
)

// Host-side configuration remains readable by the app owning the shared dir.
func inheritOwner(dir string, file *os.File) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return file.Chown(int(st.Uid), int(st.Gid))
}
