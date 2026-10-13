package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

type presetEditorModel struct {
	ID              string            `json:"id"`
	Enabled         bool              `json:"enabled"`
	WildcardRule    string            `json:"wildcard_rule"`
	Aliases         []string          `json:"aliases"`
	KeepOriginal    bool              `json:"keep_original"`
	ReasoningCap    string            `json:"reasoning_cap"`
	ReasoningLevels []string          `json:"reasoning_levels"`
	ReasoningLabels map[string]string `json:"reasoning_labels"`
}
type presetEditorChannel struct {
	Channel string              `json:"channel"`
	Label   string              `json:"label"`
	Models  []presetEditorModel `json:"models"`
}

func presetEditorJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) adminOAuthPresetEditor(w http.ResponseWriter, r *http.Request) {
	creating := r.PathValue("id") == "new"
	var preset store.OAuthPreset
	var snapshot service.OAuthPresetSnapshot
	if creating {
		presets, err := s.Store.ListOAuthPresets(r.Context())
		if err != nil {
			presetEditorJSON(w, 500, map[string]string{"error": "读取调用预设失败"})
			return
		}
		if len(presets) >= store.MaxOAuthPresets {
			presetEditorJSON(w, 409, map[string]string{"error": fmt.Sprintf("最多保存 %d 个调用预设", store.MaxOAuthPresets)})
			return
		}
		snapshot, err = s.Keys.OAuthPresetSnapshot(r.Context())
		if err != nil {
			presetEditorJSON(w, 502, map[string]string{"error": "读取当前调用配置失败，请稍后重试"})
			return
		}
	} else {
		var err error
		preset, err = s.Store.OAuthPreset(r.Context(), r.PathValue("id"))
		if err != nil {
			presetEditorJSON(w, 404, map[string]string{"error": "调用预设不存在"})
			return
		}
		if json.Unmarshal([]byte(preset.Payload), &snapshot) != nil {
			presetEditorJSON(w, 400, map[string]string{"error": "调用预设内容无效"})
			return
		}
	}
	if service.ValidateOAuthPresetFormat(snapshot) != nil {
		presetEditorJSON(w, 400, map[string]string{"error": "调用预设内容无效"})
		return
	}
	channels := []presetEditorChannel{}
	catalogChanged := false
	for _, entry := range snapshot.Channels {
		live, _, err := s.Keys.OAuthModelSettings(r.Context(), entry.Channel)
		if err != nil {
			presetEditorJSON(w, 502, map[string]string{"error": "读取 " + service.OAuthChannelLabel(entry.Channel) + " 模型目录失败，请稍后重试"})
			return
		}
		stored := map[string]service.OAuthPresetModel{}
		for _, model := range entry.Models {
			stored[strings.ToLower(model.ID)] = model
		}
		channel := presetEditorChannel{Channel: entry.Channel, Label: service.OAuthChannelLabel(entry.Channel), Models: []presetEditorModel{}}
		if len(live) != len(stored) {
			catalogChanged = true
		}
		for _, model := range live {
			key := strings.ToLower(model.ID)
			target, exists := stored[key]
			if !exists || target.WildcardRule != model.WildcardRule {
				catalogChanged = true
			}
			row := presetEditorModel{ID: model.ID, Enabled: exists && target.Enabled && model.WildcardRule == "", WildcardRule: model.WildcardRule, Aliases: []string{}, ReasoningCap: entry.ReasoningCaps[key], ReasoningLabels: map[string]string{}}
			if entry.Channel == "codex" || entry.Channel == "antigravity" {
				for _, level := range service.OAuthReasoningLevels(entry.Channel, model) {
					if level != "ultra" {
						row.ReasoningLevels = append(row.ReasoningLevels, level)
						row.ReasoningLabels[level] = reasoningEffortLabel(level)
					}
				}
			}
			for _, alias := range entry.Aliases {
				if strings.EqualFold(alias.Name, model.ID) {
					if strings.EqualFold(alias.Alias, model.ID) {
						row.KeepOriginal = true
					} else {
						row.Aliases = append(row.Aliases, alias.Alias)
					}
					row.KeepOriginal = row.KeepOriginal || alias.Fork
				}
			}
			row.KeepOriginal = row.KeepOriginal || len(row.Aliases) == 0
			channel.Models = append(channel.Models, row)
		}
		channels = append(channels, channel)
	}
	notice := ""
	if catalogChanged {
		notice = "模型目录已更新：新增模型默认关闭，已移除模型不再保存。请核对后保存。"
	}
	revision := "0"
	if !creating {
		revision = strconv.FormatInt(preset.UpdatedAt.UnixMilli(), 10)
	}
	presetEditorJSON(w, 200, map[string]any{"id": preset.ID, "name": preset.Name, "revision": revision, "creating": creating, "channels": channels, "notice": notice})
}

func (s *Server) adminOAuthPresetEdit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		presetEditorJSON(w, 400, map[string]string{"error": "预设内容过大或格式无效"})
		return
	}
	if !s.verifyCSRF(r) {
		presetEditorJSON(w, 403, map[string]string{"error": "请求已失效，请刷新页面后重试"})
		return
	}
	creating := r.PathValue("id") == "new"
	var preset store.OAuthPreset
	if !creating {
		var err error
		preset, err = s.Store.OAuthPreset(r.Context(), r.PathValue("id"))
		if err != nil {
			presetEditorJSON(w, 404, map[string]string{"error": "调用预设不存在"})
			return
		}
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if len([]rune(name)) < 1 || len([]rune(name)) > 40 {
		presetEditorJSON(w, 400, map[string]string{"error": "预设名称须为 1 至 40 个字符"})
		return
	}
	revision, err := strconv.ParseInt(r.FormValue("revision"), 10, 64)
	if !creating && (err != nil || revision != preset.UpdatedAt.UnixMilli()) {
		presetEditorJSON(w, 409, map[string]string{"error": "预设已被修改，请重新打开编辑"})
		return
	}
	action := r.FormValue("action")
	if action != "save" && action != "apply" {
		presetEditorJSON(w, 400, map[string]string{"error": "请选择保存预设或保存并应用"})
		return
	}
	var snapshot service.OAuthPresetSnapshot
	decoder := json.NewDecoder(strings.NewReader(r.FormValue("payload")))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		presetEditorJSON(w, 400, map[string]string{"error": "调用预设内容格式无效"})
		return
	}
	if err := s.Keys.ValidateOAuthPreset(r.Context(), snapshot); err != nil {
		presetEditorJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		presetEditorJSON(w, 500, map[string]string{"error": "保存调用预设失败"})
		return
	}
	preset.Name, preset.Payload, preset.UpdatedBy = name, string(payload), currentUser(r).Name
	var saved store.OAuthPreset
	if creating {
		preset.ID, err = security.NewID("preset_")
		if err == nil {
			saved, err = s.Store.CreateOAuthPreset(r.Context(), preset)
		}
	} else {
		saved, err = s.Store.UpdateOAuthPreset(r.Context(), preset, time.UnixMilli(revision))
	}
	if err != nil {
		message, status := "保存调用预设失败", 500
		if errors.Is(err, store.ErrOAuthPresetChanged) {
			message, status = "预设已被修改，请重新打开编辑", 409
		}
		if errors.Is(err, store.ErrOAuthPresetNameExists) {
			message, status = "同名预设已存在，请使用其他名称", 409
		}
		if strings.Contains(err.Error(), "limit") {
			message, status = fmt.Sprintf("最多保存 %d 个调用预设", store.MaxOAuthPresets), 409
		}
		presetEditorJSON(w, status, map[string]string{"error": message})
		return
	}
	s.audit(r, currentUser(r), "oauth_preset.save", saved.Name, "弹窗编辑；OAuth 模型、模型别名映射及思考强度上限")
	message := "调用预设已保存"
	if action == "apply" {
		if err := s.Keys.ApplyOAuthPreset(r.Context(), snapshot); err != nil {
			presetEditorJSON(w, 502, map[string]any{"error": "预设已保存，但应用失败：" + err.Error(), "revision": strconv.FormatInt(saved.UpdatedAt.UnixMilli(), 10), "saved": true, "edit_url": "/admin/upstreams/presets/" + saved.ID + "/edit"})
			return
		}
		s.audit(r, currentUser(r), "oauth_preset.apply", saved.Name, "弹窗保存并应用；OAuth 模型、模型别名映射及思考强度上限")
		message = "调用预设已保存并应用"
	}
	channel := r.FormValue("channel")
	if channel == "" {
		channel = "codex"
	}
	if normalized, err := service.NormalizeOAuthChannel(channel); err == nil {
		channel = normalized
	} else {
		channel = "codex"
	}
	presetEditorJSON(w, 200, map[string]string{"redirect": upstreamRedirectURL(channel, "msg", message)})
}
