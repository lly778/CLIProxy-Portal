package gateway

import (
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/resourcecontrol"
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

type resourceRequestKey struct{}
type resourceRequest struct {
	lease       *resourcecontrol.Lease
	transferred bool
}

func resourceInference(r *http.Request) bool {
	return isDialoguePath(r.Method, r.URL.Path) || (r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/v1beta/") || strings.HasPrefix(r.URL.Path, "/backend-api/"))) || (r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket"))
}
func (g *Gateway) serveResourceHTTP(w http.ResponseWriter, r *http.Request) {
	if g.Resources == nil || !resourceInference(r) {
		g.serveProtectedHTTP(w, r)
		return
	}
	user := "unassigned"
	key := apiKey(r)
	if key != "" {
		if owner, err := g.store.KeyByHash(r.Context(), cpamp.HashAPIKey(key)); err == nil && owner.UserID != "" {
			user = owner.UserID
		}
	}
	lease, reason := g.Resources.Acquire(user)
	if lease == nil {
		status := 429
		if reason != "global_concurrency" && reason != "user_concurrency" {
			status = 503
		}
		resourceError(w, status, reason, "服务繁忙，请稍后重试")
		return
	}
	state := &resourceRequest{lease: lease}
	defer lease.ModelDone()
	defer func() {
		if !state.transferred {
			lease.Finish()
		}
	}()
	r = r.WithContext(context.WithValue(r.Context(), resourceRequestKey{}, state))
	g.serveProtectedHTTP(w, r)
}

func resourceError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status == 429 || status == 503 {
		w.Header().Set("Retry-After", "5")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": "resource_limit", "code": code, "message": message}})
}
