package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerLeaseCannotResumeConcurrentSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "lease")
	first, err := managerLease(file)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := managerLease(file); err == nil {
		second()
		first()
		t.Fatal("concurrent snapshot/recovery accepted")
	}
	first()
	second, err := managerLease(file)
	if err != nil {
		t.Fatal("completed/interrupted snapshot lease not released:", err)
	}
	second()
}
