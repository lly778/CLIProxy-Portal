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
	client, ok := k.CPAMP.(interface {
		ListAuthFiles(context.Context) ([]cpamp.AuthFile, error)
	})
	if !ok {
		return nil, errors.New("CPAMP 客户端不支持上游账号管理")
	}
	files, err := client.ListAuthFiles(ctx)
	if err != nil {
		return nil, err
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
	channels := make([]string, 0, len(counts))
	for channel := range counts {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
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
	return rows, nil
}
