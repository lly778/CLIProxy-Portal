package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestOAuthPresetCRUDAndCaseInsensitiveOverwrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "preset-1", Name: "日常", Payload: `{"version":1}`, UpdatedBy: "甲"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "preset-2", Name: "日常", Payload: `{"version":2}`, UpdatedBy: "乙"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Payload != `{"version":2}` || second.UpdatedBy != "乙" {
		t.Fatalf("overwrite = %#v", second)
	}
	items, err := s.ListOAuthPresets(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if err := s.DeleteOAuthPreset(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OAuthPreset(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted preset err=%v", err)
	}
}

func TestOAuthPresetLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < MaxOAuthPresets; i++ {
		if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: fmt.Sprintf("id-%d", i), Name: fmt.Sprintf("name-%d", i), Payload: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "overflow", Name: "overflow", Payload: `{}`}); err == nil {
		t.Fatal("preset limit was not enforced")
	}
}
