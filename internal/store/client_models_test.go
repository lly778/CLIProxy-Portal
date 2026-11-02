package store

import (
	"errors"
	"testing"

	"cliproxy-portal/internal/domain"
)

func TestClientModelPublicationRevisionAndCorruptData(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	initial, err := s.ClientModels(ctx)
	if err != nil || initial.Enabled || initial.Revision != 0 {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	c := domain.ClientModels{Version: 1, Enabled: true, DefaultModel: "a", Models: []domain.ClientModel{{ID: "a", ReasoningLevels: []string{"high"}, DefaultReasoning: "high"}}}
	if err = s.SaveClientModels(ctx, c, 0); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveClientModels(ctx, c, 0); !errors.Is(err, ErrClientModelsConflict) {
		t.Fatalf("stale save: %v", err)
	}
	saved, err := s.ClientModels(ctx)
	if err != nil || saved.Revision != 1 || !saved.Enabled || saved.Models[0].DefaultReasoning != "high" {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	c.Enabled = false
	if err = s.SaveClientModels(ctx, c, 1); err != nil {
		t.Fatal(err)
	}
	saved, err = s.ClientModels(ctx)
	if err != nil || saved.Revision != 2 || saved.Enabled {
		t.Fatalf("disabled=%+v err=%v", saved, err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE settings SET value='invalid' WHERE key=?`, clientModelsKey); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClientModels(ctx); err == nil {
		t.Fatal("corrupt publication silently reset")
	}
	if err = s.SaveClientModels(ctx, c, 2); err == nil {
		t.Fatal("corrupt publication overwritten")
	}
}
