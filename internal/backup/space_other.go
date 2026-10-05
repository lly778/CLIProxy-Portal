//go:build !linux

package backup

// Online backup/restore runs on the Linux host; other platforms support
// archive construction and offline tests only.
func requireSpace(path string, required int64) error { return nil }
func managerLease(file string) (func(), error) {
	f, err := openManagerLease(file)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
