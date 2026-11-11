package service

import (
	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/cpamp"
	"context"
	"errors"
	"sort"
	"strings"
)

// The administrator card and shared pools read the same worker snapshot. The
// worker coalesces reads and owns the quota TTL, so neither view refreshes it on
// every page load. No account identity reaches the shared pool projection.
func (k *Keys) AGYQuota(ctx context.Context, fresh bool) (agy.QuotaSnapshot, error) {
	if k.AGY == nil {
		return agy.QuotaSnapshot{}, nil
	}
	path := "/internal/quota"
	if fresh {
		path += "?fresh=1"
	}
	var v agy.QuotaSnapshot
	err := k.AGY.Read(ctx, path, &v)
	// Both account rows and shared pools project this snapshot. Keep short
	// windows first even when Google returns weekly before five-hour quota.
	for _, group := range v.Groups {
		sort.SliceStable(group.Windows, func(i, j int) bool {
			return agyQuotaWindowOrder(group.Windows[i].Label) < agyQuotaWindowOrder(group.Windows[j].Label)
		})
	}
	if err == nil && fresh && v.Error != "" {
		err = errors.New(v.Error)
	}
	return v, err
}

func agyQuotaWindowOrder(label string) int {
	switch strings.ToUpper(strings.TrimSpace(label)) {
	case "5H":
		return 0
	case "7D":
		return 1
	case "月":
		return 2
	default:
		return 3
	}
}

func (k *Keys) agyQuotaPool(ctx context.Context) UpstreamQuotaPool {
	pool := UpstreamQuotaPool{Provider: "Antigravity CLI（ACP）"}
	v, err := k.AGYQuota(ctx, false)
	if err != nil || !v.Account.LoggedIn {
		return pool
	}
	pool.TotalAccounts = 1
	known := false
	for _, g := range v.Groups {
		available := len(g.Windows) > 0 && !v.Stale && v.Error == ""
		for _, w := range g.Windows {
			if w.RemainingPercent <= 0 {
				available = false
			}
		}
		for _, w := range g.Windows {
			period, label := "", w.Label
			switch strings.ToUpper(w.Label) {
			case "5H":
				period, label = "five_hour", "5 小时额度"
			case "7D":
				period, label = "weekly", "周额度"
			case "月":
				period, label = "monthly", "月额度"
			}
			group := UpstreamQuotaGroup{Label: g.Label + " · " + label, Period: period, KnownAccounts: 1, RemainingPercent: w.RemainingPercent, NextResetAt: w.ResetAt, ObservedAt: v.UpdatedAt}
			if available {
				group.AvailableAccounts = 1
				pool.UsableAccounts = 1
			}
			pool.Groups = append(pool.Groups, group)
			known = true
		}
	}
	if !known || v.Stale || v.Error != "" {
		pool.UnknownCount = 1
	}
	return pool
}

func (k *Keys) AGYVisibleModels(ctx context.Context, userID string) ([]cpamp.Model, error) {
	if k.AGY == nil {
		return nil, nil
	}
	settings, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, e
	}
	account, e := k.agyAccount(ctx)
	if e != nil || !account.LoggedIn {
		return nil, e
	}
	if userID != "" {
		u, e := k.Store.UserByID(ctx, userID)
		if e != nil {
			return nil, e
		}
		if !u.CanUsePortalFeatures() {
			return nil, nil
		}
	}
	out := []cpamp.Model{}
	for _, m := range settings.Visible() {
		out = append(out, cpamp.Model{ID: m.ID})
	}
	return out, nil
}

// WithAGYVisibleModels augments an already-filtered CPA catalog. Both account
// pages share the same visibility and account cache, without fetching the full
// administrative catalog or repeating CPA reads.
func (k *Keys) WithAGYVisibleModels(ctx context.Context, userID string, models []cpamp.Model) ([]cpamp.Model, error) {
	additional, err := k.AGYVisibleModels(ctx, userID)
	out := make([]cpamp.Model, 0, len(models)+len(additional))
	seen := make(map[string]bool, cap(out))
	for _, list := range [][]cpamp.Model{models, additional} {
		for _, model := range list {
			key := strings.ToLower(strings.TrimSpace(model.ID))
			if key != "" && !seen[key] {
				seen[key] = true
				out = append(out, model)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return ModelIDLess(out[i].ID, out[j].ID) })
	return out, err
}

func (k *Keys) agyAccount(ctx context.Context) (agy.Account, error) {
	if k.AGY == nil {
		return agy.Account{}, nil
	}
	return upstreamView(cpamp.WithUpstreamReadCache(ctx), k, "agy-account", func(ctx context.Context) (agy.Account, error) {
		var account agy.Account
		err := k.AGY.Read(ctx, "/internal/account", &account)
		return account, err
	}, func(v agy.Account) agy.Account { return v })
}

// AGYAccount reuses the account-presence cache shared by model management and
// the overview. It reads login state only, never quotas or model generations.
func (k *Keys) AGYAccount(ctx context.Context) (agy.Account, error) { return k.agyAccount(ctx) }
func (k *Keys) agyCatalog(ctx context.Context) ([]ClientCatalogModel, error) {
	if k.AGY == nil {
		return nil, nil
	}
	s, e := k.Store.AGYSettings(ctx)
	if e != nil {
		return nil, e
	}
	out := []ClientCatalogModel{}
	for _, m := range s.Models {
		out = append(out, ClientCatalogModel{ID: agy.Prefix + m.ID, SupportedReasoningLevels: append([]string(nil), m.Levels...)})
	}
	for _, r := range s.Rules {
		for _, alias := range r.AllAliases() {
			for _, m := range s.Models {
				if m.ID == r.ID {
					out = append(out, ClientCatalogModel{ID: agy.Prefix + alias, SupportedReasoningLevels: append([]string(nil), m.Levels...)})
				}
			}
		}
	}
	return out, nil
}
