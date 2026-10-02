package service

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

type reconcileStatusCPAMP struct {
	cpamp.API
	err error
}

func (f *reconcileStatusCPAMP) ListAPIKeys(context.Context) ([]string, error) {
	return nil, f.err
}

func TestLastReconcileStatusTracksFailuresAndRecovery(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fake := &reconcileStatusCPAMP{}
	keys := NewKeys(db, fake)
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	keys.Now = func() time.Time { return now }
	if status := keys.LastReconcileStatus(); !status.CheckedAt.IsZero() {
		t.Fatalf("unchecked status = %+v", status)
	}
	for _, retrying := range []bool{false, true, false} {
		fake.err = nil
		if retrying {
			fake.err = errors.New("upstream unavailable")
		}
		if err := keys.Reconcile(t.Context()); (err != nil) != retrying {
			t.Fatalf("reconcile error = %v, retrying = %v", err, retrying)
		}
		status := keys.LastReconcileStatus()
		if !status.CheckedAt.Equal(now) || status.Retrying != retrying {
			t.Fatalf("completed status = %+v", status)
		}
		now = now.Add(time.Minute)
	}
}
