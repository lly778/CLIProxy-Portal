package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/store"
)

func accountsForTest(t *testing.T) (*Accounts, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := NewAccounts(st, []byte("01234567890123456789012345678901"))
	return a, st
}

func TestRegisterAuthenticateAndSession(t *testing.T) {
	a, st := accountsForTest(t)
	ctx := context.Background()
	p, err := st.LatestPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u, err := a.Register(ctx, "13800138000", "张三", "long-password-123", p.Version, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != domain.StatusPending {
		t.Fatalf("status %s", u.Status)
	}
	got, err := a.Authenticate(ctx, u.Phone, "long-password-123")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.CreateSession(ctx, got, "127.0.0.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _, err := a.ResolveSession(ctx, token)
	if err != nil || resolved.ID != u.ID {
		t.Fatalf("resolve: %v %+v", err, resolved)
	}
}

func TestAdminSessionIdleTimeout(t *testing.T) {
	a, st := accountsForTest(t)
	ctx := context.Background()
	admin, err := a.CreateAdmin(ctx, "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := a.CreateSession(ctx, admin, "", "")
	if err != nil {
		t.Fatal(err)
	}
	a.Now = func() time.Time { return time.Now().UTC().Add(31 * time.Minute) }
	if _, _, err := a.ResolveSession(ctx, token); err == nil {
		t.Fatal("idle admin session accepted")
	}
	_ = st
}
