package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

const testPresetPayload = `{"version":3,"channels":[{"channel":"codex","models":[]}]}`

func TestOAuthPresetCRUDAndCaseInsensitiveOverwrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "preset-1", Name: "日常", Payload: testPresetPayload, UpdatedBy: "甲"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "preset-2", Name: "日常", Payload: testPresetPayload, UpdatedBy: "乙"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Payload != testPresetPayload || second.UpdatedBy != "乙" {
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
		if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: fmt.Sprintf("id-%d", i), Name: fmt.Sprintf("name-%d", i), Payload: testPresetPayload}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "overflow", Name: "overflow", Payload: testPresetPayload}); err == nil {
		t.Fatal("preset limit was not enforced")
	}
	if updated, err := s.SaveOAuthPreset(ctx, OAuthPreset{ID: "replacement", Name: "NAME-0", Payload: testPresetPayload}); err != nil || updated.ID != "id-0" {
		t.Fatalf("overwrite at limit=%+v err=%v", updated, err)
	}
}

func TestOAuthPresetConcurrentSavesRespectGlobalLimit(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	results := make(chan error, MaxOAuthPresets+5)
	for i := 0; i < MaxOAuthPresets+5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.SaveOAuthPreset(t.Context(), OAuthPreset{ID: fmt.Sprintf("id-%d", i), Name: fmt.Sprintf("preset-%d", i), Payload: testPresetPayload})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if err.Error() != "OAuth preset limit reached" {
			t.Fatal(err)
		}
	}
	list, err := s.ListOAuthPresets(t.Context())
	if err != nil || len(list) != MaxOAuthPresets || success != MaxOAuthPresets {
		t.Fatalf("success=%d count=%d err=%v", success, len(list), err)
	}
}

func TestOAuthPresetSaveRejectsOldFormat(t *testing.T) {
	s := testStore(t)
	if _, err := s.SaveOAuthPreset(t.Context(), OAuthPreset{ID: "old", Name: "old", Payload: `{"version":2,"channel":"codex","models":[]}`}); err == nil {
		t.Fatal("old format saved without migration")
	}
}
