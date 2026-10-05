package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/webui"
)

func (s *Server) loadOAuthPresetViews(r *http.Request, v *webui.AdminUpstreamsView) {
	current, currentErr := s.Keys.OAuthPresetSnapshot(r.Context())
	presets, err := s.Store.ListOAuthPresets(r.Context())
	if err != nil {
		v.PresetError = "调用预设暂时不可用"
		s.Logger.Error("list OAuth presets", "error", err)
		return
	}
	for _, preset := range presets {
		var snapshot service.OAuthPresetSnapshot
		row := webui.OAuthPresetView{ID: preset.ID, Name: preset.Name, UpdatedAt: s.formatTime(preset.UpdatedAt), Summary: "预设内容无法解析"}
		if json.Unmarshal([]byte(preset.Payload), &snapshot) == nil && service.ValidateOAuthPresetFormat(snapshot) == nil {
			row.Applied = currentErr == nil && service.OAuthPresetSnapshotsEqual(current, snapshot)
			enabled, total, aliases, caps := 0, 0, 0, 0
			for _, entry := range snapshot.Channels {
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
			row.Summary = fmt.Sprintf("%d 个渠道 · 启用 %d/%d 个模型 · %d 条别名 · %d 个强度上限", len(snapshot.Channels), enabled, total, aliases, caps)
		}
		v.Presets = append(v.Presets, row)
	}
}
