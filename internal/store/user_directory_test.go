package store

import (
	"testing"
	"time"

	"cliproxy-portal/internal/domain"
)

func TestUserDirectoryLimitsUsersAndKeyHistory(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.CreateUser(ctx, domain.User{ID: id, Phone: id, Name: id, PasswordHash: "must-not-load", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		for index, status := range []string{"revoked", "active"} {
			if err := s.CreateKey(ctx, domain.APIKey{ID: id + status, UserID: id, Hash: id + status, Status: status, IssuedAt: now.Add(time.Duration(index) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	directory, err := s.UserDirectoryFor(ctx, []string{"a"}, []string{"breVOKed"}, false)
	if err != nil || len(directory.Users) != 2 || len(directory.Keys["a"]) != 1 || len(directory.Keys["b"]) != 2 || len(directory.Keys["c"]) != 0 || directory.Owners["brevoked"] != "b" || directory.ActiveKeys["a"].ID != "aactive" {
		t.Fatalf("directory scope=%+v err=%v", directory, err)
	}
	for _, user := range directory.Users {
		if user.PasswordHash != "" {
			t.Fatal("loaded authentication data for display")
		}
	}
	directory, err = s.UserDirectoryFor(ctx, []string{"a"}, nil, true)
	if err != nil || len(directory.Users) != 1 || len(directory.Keys["a"]) != 2 {
		t.Fatalf("selected history missing: %+v %v", directory, err)
	}
	actors, err := s.UserIdentitiesFor(ctx, []string{"b", "b", "missing"})
	if err != nil || len(actors) != 1 || actors["b"].Name != "b" || actors["b"].PasswordHash != "" {
		t.Fatalf("audit actors expanded: %+v %v", actors, err)
	}
}

func TestUserDisplayPaginationAndFilterCount(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	for index, name := range []string{"Bravo", "Alpha", "Charlie"} {
		if err := s.CreateUser(ctx, domain.User{ID: name, Phone: name, Name: name, PasswordHash: "private", Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: now.Add(time.Duration(index) * time.Second), UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.CountUsersMatching(ctx, "approved", "a")
	if err != nil || count != 3 {
		t.Fatalf("filtered count=%d err=%v", count, err)
	}
	users, err := s.ListUserDisplayPage(ctx, "approved", "a", "name_asc", 1, 1)
	if err != nil || len(users) != 1 || users[0].Name != "Bravo" || users[0].PasswordHash != "" {
		t.Fatalf("page=%+v err=%v", users, err)
	}
	if _, err := s.ListUserDisplayPage(ctx, "", "", "unsafe", 1, 0); err == nil {
		t.Fatal("unknown SQL order accepted")
	}
}
