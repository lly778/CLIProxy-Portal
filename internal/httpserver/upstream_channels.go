package httpserver

import (
	"net/http"
	"net/url"

	"cliproxy-portal/internal/service"
)

func (s *Server) upstreamChannel(w http.ResponseWriter, r *http.Request) (string, bool) {
	value := r.URL.Query().Get("channel")
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.errorPage(w, r, http.StatusBadRequest, "渠道表单无效", nil)
			return "", false
		}
		value = r.PostForm.Get("channel")
	}
	channel, err := service.NormalizeOAuthChannel(value)
	if err != nil {
		s.errorPage(w, r, http.StatusBadRequest, "OAuth 渠道无效", nil)
		return "", false
	}
	if channel != "codex" {
		channels, err := s.Keys.OAuthChannels(r.Context())
		if err != nil {
			s.errorPage(w, r, http.StatusBadGateway, "渠道列表暂时不可用", nil)
			return "", false
		}
		found := false
		for _, candidate := range channels {
			if candidate == channel {
				found = true
				break
			}
		}
		if !found {
			s.errorPage(w, r, http.StatusBadRequest, "OAuth 渠道不存在，请刷新后重试", nil)
			return "", false
		}
	}
	return channel, true
}

func upstreamRedirectURL(channel, key, message string) string {
	target := "/admin/upstreams?" + key + "=" + url.QueryEscape(message)
	if channel != "codex" {
		target += "&channel=" + url.QueryEscape(channel)
	}
	return target
}
