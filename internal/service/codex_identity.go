package service

import (
	"context"
	"strings"
	"time"
)

// Resolve current routing, including aliases, before changing instructions.
// A name served by Codex, an unknown route, or a partially enabled shared pool
// must keep its original instructions. Do not infer providers from name prefixes
// or retain a stale alias cache when administrators change routing.
func (k *Keys) ShouldRemoveGPT5Identity(ctx context.Context, model string, enabled map[string]bool) (bool, error) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" || len(model) > 256 || len(enabled) == 0 {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	channels, err := k.oauthChannels(ctx, true)
	if err != nil {
		return false, err
	}
	found := false
	for _, channel := range channels {
		models, _, err := k.OAuthModelSettings(ctx, channel)
		if err != nil {
			return false, err
		}
		aliases, _, err := k.OAuthModelAliases(ctx, channel)
		if err != nil {
			return false, err
		}
		for _, name := range oauthVisibleNames(models, aliases) {
			if !strings.EqualFold(name.name, model) {
				continue
			}
			if channel == "codex" || !enabled[channel] {
				return false, nil
			}
			found = true
		}
	}
	return found, nil
}
