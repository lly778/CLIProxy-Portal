package httpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/webui"
)

type presetViewsCache struct {
	mu        sync.Mutex
	signature string
	views     []webui.OAuthPresetView
}

func clonePresetViews(views []webui.OAuthPresetView) []webui.OAuthPresetView {
	views = append([]webui.OAuthPresetView(nil), views...)
	for index := range views {
		view := &views[index]
		view.EnabledModels = append([]string(nil), view.EnabledModels...)
		view.DisabledModels = append([]string(nil), view.DisabledModels...)
		view.AliasMappings = append([]string(nil), view.AliasMappings...)
		view.OriginalModels = append([]string(nil), view.OriginalModels...)
		view.ReasoningCaps = append([]string(nil), view.ReasoningCaps...)
	}
	return views
}

func (s *Server) loadOAuthPresetViews(r *http.Request, v *webui.AdminUpstreamsView) {
	presets, err := s.Store.ListOAuthPresets(r.Context())
	if err != nil {
		v.PresetError = "调用预设暂时不可用"
		s.Logger.Error("list OAuth presets", "error", err)
		return
	}
	if len(presets) == 0 {
		return
	}
	current, currentErr := s.Keys.OAuthPresetSnapshot(r.Context())
	present, presenceErr := s.Keys.OAuthAccountChannels(r.Context())
	if presenceErr != nil {
		v.PresetError = "上游账号暂时不可用"
		return
	}
	// Compare exact current inputs, rather than keeping a time-based database cache.
	// JSON parsing, sorting and applied-state comparisons are shared by unchanged visits.
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%t\n", currentErr == nil)
	_ = json.NewEncoder(digest).Encode(current)
	_ = json.NewEncoder(digest).Encode(present)
	for _, preset := range presets {
		for _, value := range []string{preset.ID, preset.Name, preset.Payload} {
			_, _ = fmt.Fprintf(digest, "%d:%s", len(value), value)
		}
		_, _ = fmt.Fprintf(digest, ":%d\n", preset.UpdatedAt.UnixMilli())
	}
	signature := fmt.Sprintf("%x", digest.Sum(nil))
	cache := &s.presetViews
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.signature == signature {
		v.Presets = clonePresetViews(cache.views)
		return
	}
	for _, preset := range presets {
		var snapshot service.OAuthPresetSnapshot
		row := webui.OAuthPresetView{ID: preset.ID, Name: preset.Name, UpdatedAt: s.formatTime(preset.UpdatedAt), Summary: "预设内容无法解析"}
		if json.Unmarshal([]byte(preset.Payload), &snapshot) == nil && service.ValidateOAuthPresetFormat(snapshot) == nil {
			row.Applied = currentErr == nil && service.OAuthPresetSnapshotsEqual(current, snapshot)
			sort.SliceStable(snapshot.Channels, func(i, j int) bool {
				return service.UpstreamChannelLess(snapshot.Channels[i].Channel, snapshot.Channels[j].Channel)
			})
			enabled, total, aliases, caps, visibleChannels := 0, 0, 0, 0, 0
			for _, entry := range snapshot.Channels {
				if present != nil && !present[entry.Channel] {
					continue
				}
				visibleChannels++
				prefix := service.OAuthChannelLabel(entry.Channel) + " · "
				total += len(entry.Models)
				aliases += len(entry.Aliases)
				caps += len(entry.ReasoningCaps)
				canonical := map[string]string{}
				aliasCount, keepOriginal := map[string]int{}, map[string]bool{}
				for _, alias := range entry.Aliases {
					row.AliasMappings = append(row.AliasMappings, prefix+alias.Alias+" → "+alias.Name)
					id := strings.ToLower(alias.Name)
					aliasCount[id]++
					keepOriginal[id] = keepOriginal[id] || alias.Fork
				}
				for _, model := range entry.Models {
					id := strings.ToLower(model.ID)
					canonical[id] = model.ID
					if model.Enabled {
						enabled++
						row.EnabledModels = append(row.EnabledModels, prefix+model.ID)
					} else {
						row.DisabledModels = append(row.DisabledModels, prefix+model.ID)
					}
					if model.Enabled && (aliasCount[id] == 0 || keepOriginal[id]) {
						row.OriginalModels = append(row.OriginalModels, prefix+model.ID)
					}
				}
				capModels := []string{}
				for model := range entry.ReasoningCaps {
					capModels = append(capModels, model)
				}
				sort.Strings(capModels)
				for _, model := range capModels {
					name := canonical[strings.ToLower(model)]
					if name == "" {
						name = model
					}
					row.ReasoningCaps = append(row.ReasoningCaps, prefix+name+" · "+reasoningEffortLabel(entry.ReasoningCaps[model]))
				}
			}
			row.Summary = fmt.Sprintf("%d 个渠道 · 启用 %d/%d 个模型 · %d 条别名 · %d 个强度上限", visibleChannels, enabled, total, aliases, caps)
		}
		v.Presets = append(v.Presets, row)
	}
	cache.signature, cache.views = signature, clonePresetViews(v.Presets)
}
