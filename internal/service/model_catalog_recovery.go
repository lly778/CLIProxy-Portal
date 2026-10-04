package service

import (
	"context"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

// RestoreSchedulableModels repairs CPA's stale catalog projection after an
// Antigravity 403 cooldown. The scheduler expires the deadline, but its model
// registry can keep Unavailable set until the next successful request. Read
// the current credential snapshot and its registered models instead; do not
// reset routing state on a GET, bypass active cooldowns, or invent static IDs.
func (k *Keys) RestoreSchedulableModels(ctx context.Context, models []cpamp.Model) []cpamp.Model {
	client, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
		ListAuthFileModels(context.Context, cpamp.AuthFile) ([]cpamp.Model, error)
	})
	if !ok {
		return models
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		return models
	}
	result := append([]cpamp.Model(nil), models...)
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		seen[strings.ToLower(model.ID)] = true
	}
	identities := map[string]bool{}
	now := k.Now()
	for _, file := range files {
		if !strings.EqualFold(file.Provider, "antigravity") || file.Disabled || file.Unavailable ||
			!file.AvailabilityKnown || !strings.EqualFold(file.Status, "active") || file.AuthIndex == "" ||
			!authOutsideCooldown(file.Cooldowns, now) {
			continue
		}
		if identities[file.AuthIndex] {
			continue
		}
		if len(identities) >= 20 || ctx.Err() != nil {
			break
		}
		identities[file.AuthIndex] = true
		registered, err := client.ListAuthFileModels(ctx, file)
		if err != nil {
			continue // A failed management read never fabricates availability.
		}
		for _, model := range registered {
			id := strings.TrimSpace(model.ID)
			if id == "" || len(id) > 256 || strings.ContainsAny(id, "\r\n\x00") || seen[strings.ToLower(id)] {
				continue
			}
			model.ID = id
			if model.Object == "" {
				model.Object = "model"
			}
			if model.OwnedBy == "" {
				model.OwnedBy = file.Provider
			}
			result = append(result, model)
			seen[strings.ToLower(id)] = true
		}
	}
	return result
}

func authOutsideCooldown(cooldowns []cpamp.AuthCooldown, now time.Time) bool {
	for _, cooldown := range cooldowns {
		// A registered route may be an alias or share a quota group. Without
		// guessing that mapping, only recover this credential when every
		// reported cooldown has ended. Existing upstream entries stay intact.
		if cooldown.RetryAt.IsZero() || cooldown.RetryAt.After(now) {
			return false
		}
	}
	return true
}
