package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cliproxy-portal/internal/domain"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestUserKeyAndAnonymize(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := domain.User{ID: "usr_test", Phone: "13800138000", Name: "张三", PasswordHash: "hash", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateKey(ctx, domain.APIKey{ID: "key_test", UserID: u.ID, Hash: "abc", LastFour: "1234", Alias: "张三-8000-ABCD", Status: "active", IssuedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveKey(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.AnonymizeUser(ctx, u.ID, "已删除用户-ABCD"); err != nil {
		t.Fatal(err)
	}
	got, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phone != "" || got.PasswordHash != "" || got.Status != domain.StatusDeleted {
		t.Fatalf("not anonymized: %+v", got)
	}
}

func TestOnlyOneActiveKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := domain.User{ID: "u", Phone: "13900139000", Name: "李四", PasswordHash: "h", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	for i, hash := range []string{"one", "two"} {
		err := s.CreateKey(ctx, domain.APIKey{ID: string(rune('a' + i)), UserID: u.ID, Hash: hash, LastFour: "1234", Alias: "a", Status: "active", IssuedAt: now})
		if i == 0 && err != nil {
			t.Fatal(err)
		}
		if i == 1 && err == nil {
			t.Fatal("second active key accepted")
		}
	}
}

func TestDemoteAdminPreservesOneApprovedAdministrator(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, user := range []domain.User{
		{ID: "admin-one", Phone: "13900139001", Name: "管理员一", PasswordHash: "h", Role: domain.RoleAdmin, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now},
		{ID: "admin-two", Phone: "13900139002", Name: "管理员二", PasswordHash: "h", Role: domain.RoleAdmin, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := s.DemoteAdmin(ctx, "admin-two")
	if err != nil || !changed {
		t.Fatalf("first demotion changed=%v err=%v", changed, err)
	}
	changed, err = s.DemoteAdmin(ctx, "admin-one")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("last approved administrator was demoted")
	}
}

func TestListAuditForUserExcludesOtherUsers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	events := []domain.AuditEvent{
		{ActorUserID: "admin", ActorLabel: "管理员", Action: "user.approve", TargetID: "user-a", TargetLabel: "用户 A", CreatedAt: now},
		{ActorUserID: "user-a", ActorLabel: "用户 A", Action: "key.issue", TargetID: "user-a", TargetLabel: "用户 A", CreatedAt: now.Add(time.Second)},
		{ActorUserID: "user-b", ActorLabel: "用户 B", Action: "key.issue", TargetID: "user-b", TargetLabel: "用户 B", CreatedAt: now.Add(2 * time.Second)},
	}
	for _, event := range events {
		if err := s.Audit(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListAuditForUser(ctx, "user-a", 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("user audit count = %d, want 2: %#v", len(got), got)
	}
	for _, event := range got {
		if event.ActorUserID == "user-b" || event.TargetID == "user-b" {
			t.Fatalf("unrelated audit event leaked: %#v", event)
		}
	}
}

func TestPasswordResetSingleUse(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := domain.User{ID: "u", Phone: "13700137000", Name: "王五", PasswordHash: "h", Role: domain.RoleUser, Status: domain.StatusPending, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	r := PasswordReset{CodeHash: "code", UserID: u.ID, ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err := s.CreatePasswordReset(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.UsePasswordReset(ctx, "code"); err != nil {
		t.Fatal(err)
	}
	if err := s.UsePasswordReset(ctx, "code"); err == nil {
		t.Fatal("reset reused")
	}
}
