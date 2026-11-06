package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cliproxy-portal/internal/clientsync"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func clientConfigJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) clientModelsGet(w http.ResponseWriter, r *http.Request) {
	if !s.limit.Allow("client-models-public:"+s.clientIP(r), 60, time.Minute, time.Minute) {
		clientConfigJSON(w, http.StatusTooManyRequests, map[string]string{"error": "获取过于频繁，请稍后再试"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	config, err := s.Keys.PublishedClientModels(ctx)
	if err != nil {
		status, message := http.StatusInternalServerError, "客户端模型配置暂时无法读取"
		if errors.Is(err, service.ErrClientModelsUnavailable) {
			status, message = http.StatusNotFound, err.Error()
		}
		// Return a safe message, never upstream response bodies or credentials.
		clientConfigJSON(w, status, map[string]string{"error": message})
		return
	}
	config.APIBaseURL = strings.TrimRight(s.Cfg.CPAAPIBaseURL, "/")
	if config.APIBaseURL != "" && !strings.HasSuffix(config.APIBaseURL, "/v1") {
		config.APIBaseURL += "/v1"
	}
	clientConfigJSON(w, http.StatusOK, config)
}

func (s *Server) downloadClientSync(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="Sync-Codex.ps1"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(clientsync.ScriptForPortal(s.Cfg.ExternalURL))
}

func (s *Server) downloadClientSyncBatch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="Sync-Codex.bat"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(clientsync.BatchForPortal(s.Cfg.ExternalURL))
}

func (s *Server) loadClientModelView(r *http.Request, v *webui.AdminUpstreamsView) {
	config, err := s.Store.ClientModels(r.Context())
	if err != nil {
		v.ClientModels.Error = "客户端模型配置暂时无法读取"
		return
	}
	v.ClientModels.Revision = strconv.FormatInt(config.Revision, 10)
	v.ClientModels.DefaultModel = config.DefaultModel
	v.ClientModels.Ready = true
	// The complete catalog is fetched only when the model picker opens.
	for _, model := range config.Models {
		row := webui.ClientModelRowView{ID: model.ID, Selected: true, DefaultReasoning: model.DefaultReasoning}
		if model.ID == config.DefaultModel {
			v.ClientModels.DefaultReasoning = model.DefaultReasoning
		}
		row.DefaultOptions = append(row.DefaultOptions, webui.ReasoningCapOptionView{Value: "", Label: "沿用模型默认", Selected: model.DefaultReasoning == ""})
		for _, level := range domain.ClientReasoningLevels() {
			row.DefaultOptions = append(row.DefaultOptions, webui.ReasoningCapOptionView{Value: level, Label: level, Selected: model.DefaultReasoning == level})
		}
		v.ClientModels.Models = append(v.ClientModels.Models, row)
	}
}

func (s *Server) adminClientModelCatalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	models, err := s.Keys.ClientModelCatalog(ctx)
	if err != nil {
		clientConfigJSON(w, http.StatusBadGateway, map[string]string{"error": "完整模型目录暂时无法获取，请稍后重试；已选模型保持不变"})
		return
	}
	ids := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		ids = append(ids, model.ID)
		seen[model.ID] = true
	}
	config, err := s.Store.ClientModels(ctx)
	if err != nil {
		clientConfigJSON(w, http.StatusInternalServerError, map[string]string{"error": "无法读取已保存的客户端配置"})
		return
	}
	for _, model := range config.Models {
		if !seen[model.ID] {
			ids = append(ids, model.ID)
		}
	}
	clientConfigJSON(w, http.StatusOK, map[string]any{"models": ids})
}

func (s *Server) adminClientModelsSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "客户端模型表单无效或过大", nil)
		return
	}
	if !s.verifyCSRF(r) {
		s.errorPage(w, r, http.StatusForbidden, "请求已失效", nil)
		return
	}
	revision, err := strconv.ParseInt(r.PostForm.Get("client_revision"), 10, 64)
	if err != nil || revision < 0 {
		s.errorPage(w, r, http.StatusBadRequest, "配置版本无效，请刷新页面", nil)
		return
	}
	config := domain.ClientModels{Version: domain.ClientModelsVersion, Enabled: true, DefaultModel: r.PostForm.Get("default_model"), Models: []domain.ClientModel{}}
	chosen := map[string]bool{}
	for _, id := range r.PostForm["include_model"] {
		if chosen[id] {
			s.errorPage(w, r, http.StatusBadRequest, "模型选择重复", nil)
			return
		}
		chosen[id] = true
	}
	for index, id := range r.PostForm["client_model_id"] {
		if !chosen[id] {
			continue
		}
		config.Models = append(config.Models, domain.ClientModel{ID: id, DefaultReasoning: r.PostForm.Get(fmt.Sprintf("default_%d", index))})
		delete(chosen, id)
	}
	if len(chosen) != 0 {
		s.errorPage(w, r, http.StatusBadRequest, "模型选择与表单不一致", nil)
		return
	}
	if err = config.Validate(); err != nil {
		s.errorPage(w, r, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if config.Enabled {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		catalog, listErr := s.Keys.ClientModelCatalog(ctx)
		if listErr != nil {
			s.errorPage(w, r, http.StatusBadGateway, "无法获取完整模型目录，配置未保存，请稍后重试", nil)
			return
		}
		set := map[string]bool{}
		for _, model := range catalog {
			set[model.ID] = true
		}
		previous, readErr := s.Store.ClientModels(ctx)
		if readErr != nil {
			s.errorPage(w, r, http.StatusInternalServerError, "无法读取原配置，未保存", readErr)
			return
		}
		for _, model := range previous.Models {
			set[model.ID] = true
		}
		for _, model := range config.Models {
			if !set[model.ID] {
				s.errorPage(w, r, http.StatusBadRequest, "模型不在已知目录中，请重新获取模型列表："+model.ID, nil)
				return
			}
		}
	}
	if err = s.Store.SaveClientModels(r.Context(), config, revision); err != nil {
		if errors.Is(err, store.ErrClientModelsConflict) {
			s.errorPage(w, r, http.StatusConflict, err.Error(), nil)
		} else {
			s.errorPage(w, r, http.StatusInternalServerError, "保存客户端模型配置失败", err)
		}
		return
	}
	s.audit(r, currentUser(r), "client_models.save", "client-models", fmt.Sprintf("模型数 %d", len(config.Models)))
	channel, _ := service.NormalizeOAuthChannel(r.PostForm.Get("channel"))
	http.Redirect(w, r, upstreamRedirectURL(channel, "msg", "客户端模型配置已保存")+"#client-models", http.StatusSeeOther)
}
