package backup

import "io"

const backupSpaceReserve int64 = 128 << 20

// Check the actual remaining space before each encrypted output write. The
// archive size is data-dependent, so preflight cannot assume a compression ratio.
type spaceCheckedWriter struct {
	writer io.Writer
	check  func(required int64) error
}

func (w *spaceCheckedWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := w.check(int64(len(p)) + backupSpaceReserve); err != nil {
		return 0, err
	}
	n, err := w.writer.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}
