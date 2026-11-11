package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"cliproxy-portal/internal/cpamp"
)

type EnabledUpstream struct {
	Channel     string
	Accounts    int
	Models      []string
	ModelsReady bool
}

// EnabledUpstreams reads configuration only; it never probes provider quotas
// or sends model requests from the overview page.
func (k *Keys) EnabledUpstreams(ctx context.Context) ([]EnabledUpstream, error) {
	_, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	var files []cpamp.AuthFile
	var summaryErr error
	if ok {
		files, summaryErr = k.upstreamAuthFiles(ctx)
	} else if k.AGY == nil {
		summaryErr = errors.New("CPAMP 客户端不支持上游账号管理")
	}
	counts := map[string]int{}
	seen := map[string]bool{}
	for _, file := range files {
		if file.Disabled || strings.TrimSpace(file.Name) == "" || strings.TrimSpace(file.Provider) == "" {
			continue
		}
		channel, err := NormalizeOAuthChannel(file.Provider)
		if err != nil {
			continue
		}
		id := channel + ":" + upstreamAccountID(file)
		if seen[id] {
			continue
		}
		seen[id] = true
		counts[channel]++
	}
	// The CLI account is independent of CPA's credentials. Reuse the same
	// cached account/configuration sources as upstream management, never quota
	// discovery or a full model catalog fetch from the overview page.
	if k.AGY != nil {
		account, err := k.agyAccount(ctx)
		if err != nil {
			summaryErr = errors.Join(summaryErr, err)
		} else if account.LoggedIn {
			counts["agy"] = 1
		}
	}
	channels := make([]string, 0, len(counts))
	for channel := range counts {
		channels = append(channels, channel)
	}
	sort.Slice(channels, func(i, j int) bool { return UpstreamChannelLess(channels[i], channels[j]) })
	rows := make([]EnabledUpstream, 0, len(channels))
	for _, channel := range channels {
		row := EnabledUpstream{Channel: channel, Accounts: counts[channel]}
		models, _, modelErr := k.OAuthModelSettings(ctx, channel)
		row.ModelsReady = modelErr == nil
		for _, model := range models {
			if model.Enabled {
				row.Models = append(row.Models, model.ID)
			}
		}
		rows = append(rows, row)
	}
	return rows, summaryErr
}
