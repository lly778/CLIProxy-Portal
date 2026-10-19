package backup

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupSpaceRequiredIncludesWALWithoutTriplingSnapshots(t *testing.T) {
	sources, dir := fixture(t)
	var sourceBytes int64
	for _, name := range []string{"portal.db", "portal.db-wal", "config.yaml", "auths/account.json"} {
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		sourceBytes += info.Size()
	}
	if sourceBytes == 0 {
		t.Fatal("empty fixture")
	}
	required, err := backupSpaceRequired(sources)
	if err != nil {
		t.Fatal(err)
	}
	// A disk with room for a snapshot and the reserve must pass preflight even
	// when it cannot hold three complete copies. Archive growth is checked later.
	budget := sourceBytes + backupSpaceReserve
	if required != budget {
		t.Fatalf("required=%d, snapshot+WAL+reserve=%d", required, budget)
	}
	if required >= 3*sourceBytes+backupSpaceReserve {
		t.Fatal("obsolete three-copy requirement retained")
	}
}

func TestSpaceCheckedWriterStopsEncryptionBeforeConsumingReserve(t *testing.T) {
	var output bytes.Buffer
	spaceErr := errors.New("simulated disk reserve reached")
	budget := backupSpaceReserve + chunkSize + 64
	checks := 0
	w := &spaceCheckedWriter{writer: &output, check: func(required int64) error {
		checks++
		if required > budget-int64(output.Len()) {
			return spaceErr
		}
		return nil
	}}
	key := bytes.Repeat([]byte{3}, 32)
	err := Encrypt(w, bytes.NewReader(bytes.Repeat([]byte{5}, chunkSize*3)), key)
	if !errors.Is(err, spaceErr) {
		t.Fatalf("space failure not propagated: %v", err)
	}
	if checks < 3 || output.Len() == 0 || int64(output.Len()) > budget-backupSpaceReserve {
		t.Fatalf("reserve consumed: checks=%d written=%d budget=%d", checks, output.Len(), budget)
	}
	if err = Decrypt(io.Discard, bytes.NewReader(output.Bytes()), key); err == nil {
		t.Fatal("space-aborted output accepted as a complete backup")
	}
}

func TestSpaceCheckedWriterPreservesErrorsAndRefusesUncheckedWrites(t *testing.T) {
	var output bytes.Buffer
	checkErr := errors.New("cannot read filesystem space")
	w := &spaceCheckedWriter{writer: &output, check: func(int64) error { return checkErr }}
	if n, err := w.Write([]byte("secret")); n != 0 || !errors.Is(err, checkErr) || output.Len() != 0 {
		t.Fatalf("wrote despite failed space check: n=%d err=%v", n, err)
	}
	writeErr := errors.New("disk write failed")
	w = &spaceCheckedWriter{writer: failingSpaceTestWriter{err: writeErr}, check: func(int64) error { return nil }}
	if _, err := w.Write([]byte("secret")); !errors.Is(err, writeErr) {
		t.Fatalf("write error lost: %v", err)
	}
	w.writer = failingSpaceTestWriter{}
	if _, err := w.Write([]byte("secret")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write accepted: %v", err)
	}
}

type failingSpaceTestWriter struct{ err error }

func (w failingSpaceTestWriter) Write([]byte) (int, error) { return 0, w.err }
