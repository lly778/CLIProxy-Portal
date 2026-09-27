package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
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

func TestSessionLifetimeDependsOnRememberNotRole(t *testing.T) {
	a, st := accountsForTest(t)
	ctx := context.Background()
	p, err := st.LatestPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user, err := a.Register(ctx, "13800138000", "用户", "long-password-123", p.Version, "")
	if err != nil {
		t.Fatal(err)
	}
	admin, err := a.CreateAdmin(ctx, "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []domain.User{user, admin} {
		for _, remember := range []bool{false, true} {
			max, idle := 8*time.Hour, 30*time.Minute
			if remember {
				max, idle = 30*24*time.Hour, 7*24*time.Hour
			}
			for _, expireByIdle := range []bool{false, true} {
				start := time.Now().UTC().Truncate(time.Millisecond)
				a.Now = func() time.Time { return start }
				token, _, err := a.CreateSessionWithRemember(ctx, u, "", "", remember)
				if err != nil {
					t.Fatal(err)
				}
				sess, err := st.Session(ctx, security.SHA256(token))
				if err != nil || sess.Remember != remember || sess.ExpiresAt.Sub(start) != max {
					t.Fatalf("role=%s remember=%v session=%+v err=%v", u.Role, remember, sess, err)
				}
				limit := max
				if expireByIdle {
					limit = idle
				} else {
					if err := st.TouchSession(ctx, sess.TokenHash, start.Add(max-time.Second)); err != nil {
						t.Fatal(err)
					}
				}
				a.Now = func() time.Time { return start.Add(limit - time.Millisecond) }
				if _, _, err := a.ResolveSession(ctx, token); err != nil {
					t.Fatalf("session expired early: %v", err)
				}
				// Restore LastSeen because a successful resolution refreshes it.
				if expireByIdle {
					if err := st.TouchSession(ctx, sess.TokenHash, start); err != nil {
						t.Fatal(err)
					}
				}
				a.Now = func() time.Time { return start.Add(limit) }
				if _, _, err := a.ResolveSession(ctx, token); err == nil {
					t.Fatalf("role=%s remember=%v idle=%v accepted expired session", u.Role, remember, expireByIdle)
				}
			}
		}
	}
}

func TestChangePhoneRequiresAdminRoleWithoutPasswordReauthentication(t *testing.T) {
	a, st := accountsForTest(t)
	ctx := context.Background()
	admin, err := a.CreateAdmin(ctx, "13900139000", "管理员", "very-long-admin-password", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := st.LatestPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	user, err := a.Register(ctx, "13800138000", "张三", "long-password-123", policy.Version, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ChangePhone(ctx, admin, user, "13700137000", "127.0.0.1"); err != nil {
		t.Fatalf("administrator changed phone: %v", err)
	}
	updated, err := st.UserByID(ctx, user.ID)
	if err != nil || updated.Phone != "13700137000" {
		t.Fatalf("updated user = %#v, err = %v", updated, err)
	}
	if err = a.ChangePhone(ctx, user, admin, "13600136000", "127.0.0.1"); err == nil {
		t.Fatal("ordinary user changed another account phone")
	}
}
