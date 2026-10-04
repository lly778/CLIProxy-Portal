package service

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"cliproxy-portal/internal/cpamp"
)

func (k *Keys) fetchAntigravityQuota(ctx context.Context, file cpamp.AuthFile, now time.Time) error {
	client, ok := k.CPAMP.(interface {
		FetchAntigravityQuota(context.Context, cpamp.AuthFile, time.Time) ([]cpamp.AntigravityQuotaWindow, error)
	})
	if !ok {
		return errors.New("CPAMP 客户端不支持 Antigravity 额度查询")
	}
	windows, err := client.FetchAntigravityQuota(ctx, file, now)
	k.refreshMu.Lock()
	defer k.refreshMu.Unlock()
	if k.antigravityQuota == nil {
		k.antigravityQuota = map[string][]cpamp.AntigravityQuotaWindow{}
	}
	if k.antigravityAttempt == nil {
		k.antigravityAttempt = map[string]time.Time{}
	}
	id := upstreamAccountID(file)
	k.antigravityAttempt[id] = now
	if err != nil || len(windows) == 0 {
		delete(k.antigravityQuota, id)
		if err == nil {
			err = errors.New("Antigravity 未返回可用额度")
		}
		return err
	}
	k.antigravityQuota[id] = append([]cpamp.AntigravityQuotaWindow(nil), windows...)
	return nil
}

func (k *Keys) antigravityAccountQuotas(ctx context.Context) (map[string]UpstreamAccountQuota, error) {
	client, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持读取上游账号")
	}
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		return nil, err
	}
	return k.antigravityQuotasForFiles(ctx, files)
}

func (k *Keys) antigravityQuotasForFiles(ctx context.Context, files []cpamp.AuthFile) (map[string]UpstreamAccountQuota, error) {
	k.quotaFetchMu.Lock()
	defer k.quotaFetchMu.Unlock()
	result := map[string]UpstreamAccountQuota{}
	seen := map[string]bool{}
	now := k.Now()
	var lastErr error
	for _, file := range files {
		if file.Disabled || !strings.EqualFold(strings.TrimSpace(file.Provider), "antigravity") || strings.TrimSpace(file.Name) == "" {
			continue
		}
		id := upstreamAccountID(file)
		if seen[id] {
			continue
		}
		if len(seen) >= 200 {
			break
		}
		seen[id] = true
		k.refreshMu.Lock()
		windows := append([]cpamp.AntigravityQuotaWindow(nil), k.antigravityQuota[id]...)
		attempt := k.antigravityAttempt[id]
		k.refreshMu.Unlock()
		fresh := len(windows) > 0
		for _, w := range windows {
			if w.ObservedAt.After(now) || now.Sub(w.ObservedAt) > k.QuotaCacheTTL || (!w.ResetAt.IsZero() && w.ResetAt.After(w.ObservedAt) && !w.ResetAt.After(now)) {
				fresh = false
				break
			}
		}
		if !fresh {
			windows = nil
			if attempt.IsZero() || now.Sub(attempt) >= time.Minute {
				if err := k.fetchAntigravityQuota(ctx, file, now); err != nil {
					lastErr = err
				} else {
					k.refreshMu.Lock()
					windows = append([]cpamp.AntigravityQuotaWindow(nil), k.antigravityQuota[id]...)
					k.refreshMu.Unlock()
				}
			} else {
				lastErr = errors.New("Antigravity 额度查询未完成，请稍后刷新")
			}
		}
		if len(windows) == 0 {
			continue
		}
		quota := UpstreamAccountQuota{AccountID: id}
		groupAvailable := map[string]bool{}
		for _, w := range windows {
			group := w.GroupID
			if group == "" {
				group = w.ID
			}
			available, seen := groupAvailable[group]
			groupAvailable[group] = (!seen || available) && w.RemainingPercent > 0
		}
		for _, w := range windows {
			group := w.GroupID
			if group == "" {
				group = w.ID
			}
			quota.Windows = append(quota.Windows, UpstreamAccountQuotaWindow{Label: w.Label, Available: groupAvailable[group], Period: w.ID, RemainingPercent: int(math.Round(w.RemainingPercent)), ResetAt: w.ResetAt, ObservedAt: w.ObservedAt})
		}
		result[id] = quota
		if ctx.Err() != nil {
			break
		}
	}
	return result, lastErr
}

// UpstreamQuotas keeps providers separate and returns partial results even if
// one provider is unavailable. Shared views never receive credential identities.
func (k *Keys) UpstreamQuotas(ctx context.Context) []UpstreamQuotaPool {
	pools := []UpstreamQuotaPool{}
	if pool, _ := k.UpstreamQuota(ctx); pool.TotalAccounts > 0 {
		pools = append(pools, pool)
	}
	client, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	if !ok {
		return pools
	}
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		return pools
	}
	pool := UpstreamQuotaPool{Provider: "Antigravity"}
	seen := map[string]bool{}
	for _, file := range files {
		if file.Disabled || !strings.EqualFold(strings.TrimSpace(file.Provider), "antigravity") || strings.TrimSpace(file.Name) == "" {
			continue
		}
		seen[upstreamAccountID(file)] = true
		if len(seen) == 200 {
			break
		}
	}
	pool.TotalAccounts = len(seen)
	if pool.TotalAccounts == 0 {
		return pools
	}
	quotas, _ := k.antigravityQuotasForFiles(ctx, files)
	type accumulator struct {
		group UpstreamQuotaGroup
		total int
	}
	groups := map[string]*accumulator{}
	for _, quota := range quotas {
		usable := false
		for _, w := range quota.Windows {
			acc := groups[w.Period]
			if acc == nil {
				acc = &accumulator{group: UpstreamQuotaGroup{Label: w.Label, Period: w.Period}}
				groups[w.Period] = acc
			}
			acc.group.KnownAccounts++
			acc.total += w.RemainingPercent
			if w.Available {
				acc.group.AvailableAccounts++
				usable = true
			}
			if w.ResetAt.After(k.Now()) && (acc.group.NextResetAt.IsZero() || w.ResetAt.Before(acc.group.NextResetAt)) {
				acc.group.NextResetAt = w.ResetAt
			}
			if acc.group.ObservedAt.IsZero() || w.ObservedAt.Before(acc.group.ObservedAt) {
				acc.group.ObservedAt = w.ObservedAt
			}
		}
		if usable {
			pool.UsableAccounts++
		}
	}
	ids := []string{}
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		acc := groups[id]
		acc.group.RemainingPercent = int(math.Round(float64(acc.total) / float64(acc.group.KnownAccounts)))
		acc.group.Estimated = acc.group.KnownAccounts > 1
		pool.Groups = append(pool.Groups, acc.group)
	}
	pool.UnknownCount = pool.TotalAccounts - len(quotas)
	pools = append(pools, pool)
	return pools
}
