package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/security"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

type contextKey string

const userKey contextKey = "user"
const sessionKey contextKey = "session"
const tokenKey contextKey = "token"

type Server struct {
	Cfg      config.Config
	Store    *store.Store
	Accounts *service.Accounts
	Keys     *service.Keys
	UI       *webui.Renderer
	Secret   []byte
	Logger   *slog.Logger
	limit    *limiter
}

func New(cfg config.Config, st *store.Store, accounts *service.Accounts, keys *service.Keys, secret []byte, logger *slog.Logger) (*Server, error) {
	ui, err := webui.NewRenderer()
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Cfg: cfg, Store: st, Accounts: accounts, Keys: keys, UI: ui, Secret: secret, Logger: logger, limit: newLimiter()}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(webui.Assets()))))
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /login", s.loginGet)
	mux.HandleFunc("POST /login", s.loginPost)
	mux.HandleFunc("GET /register", s.registerGet)
	mux.HandleFunc("POST /register", s.registerPost)
	mux.HandleFunc("GET /reset", s.resetGet)
	mux.HandleFunc("POST /reset", s.resetPost)
	mux.HandleFunc("GET /resubmit", s.withUser(s.resubmitGet))
	mux.HandleFunc("POST /resubmit", s.withUser(s.resubmitPost))
	mux.HandleFunc("GET /rules", s.rules)
	mux.HandleFunc("POST /logout", s.withUser(s.logout))
	mux.HandleFunc("GET /", s.withUser(s.home))
	mux.HandleFunc("GET /dashboard", s.withUser(s.dashboard))
	mux.HandleFunc("GET /status", s.withUser(s.statusPage))
	mux.HandleFunc("GET /key", s.withApproved(s.keyPage))
	mux.HandleFunc("POST /key/claim", s.withApproved(s.keyClaim))
	mux.HandleFunc("POST /key/test", s.withApproved(s.keyTest))
	mux.HandleFunc("POST /key/revoke", s.withApproved(s.keyRevoke))
	mux.HandleFunc("POST /key/acknowledge", s.withApproved(s.keyAcknowledge))
	mux.HandleFunc("GET /usage", s.withApproved(s.usage))
	mux.HandleFunc("POST /quota/refresh", s.withApproved(s.quotaRefresh))
	mux.HandleFunc("GET /models", s.withApproved(s.models))
	mux.HandleFunc("POST /models/test", s.withApproved(s.modelsTest))
	mux.HandleFunc("GET /activity", s.withApproved(s.activity))
	mux.HandleFunc("GET /health", s.withUser(s.health))
	mux.HandleFunc("GET /profile", s.withUser(s.profile))
	mux.HandleFunc("GET /password", s.withUser(s.passwordGet))
	mux.HandleFunc("POST /password", s.withUser(s.passwordPost))
	mux.HandleFunc("GET /admin", s.withAdmin(s.adminDashboard))
	mux.HandleFunc("GET /admin/usage", s.withAdmin(s.adminUsage))
	mux.HandleFunc("GET /admin/users", s.withAdmin(s.adminUsers))
	mux.HandleFunc("GET /admin/users/{id}", s.withAdmin(s.adminUser))
	mux.HandleFunc("POST /admin/users/{id}/{action}", s.withAdmin(s.adminUserAction))
	mux.HandleFunc("POST /admin/approvals/{id}/{action}", s.withAdmin(s.adminApprovalAction))
	mux.HandleFunc("GET /admin/approvals", s.withAdmin(s.adminApprovals))
	mux.HandleFunc("GET /admin/admins", s.withAdmin(s.adminAdmins))
	mux.HandleFunc("POST /admin/admins", s.withAdmin(s.adminCreate))
	mux.HandleFunc("POST /admin/admins/{id}/disable", s.withAdmin(s.adminDisable))
	mux.HandleFunc("GET /admin/policy", s.withAdmin(s.adminPolicy))
	mux.HandleFunc("POST /admin/policy", s.withAdmin(s.adminPolicySave))
	mux.HandleFunc("POST /admin/registration", s.withAdmin(s.adminRegistration))
	mux.HandleFunc("GET /admin/audit", s.withAdmin(s.adminAudit))
	mux.HandleFunc("GET /admin/health", s.withAdmin(s.adminHealth))
	mux.HandleFunc("POST /admin/health/check", s.withAdmin(s.adminHealth))
	return s.recover(s.headers(s.logRequests(mux)))
}

func (s *Server) withUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := s.sessionToken(r)
		u, sess, err := s.Accounts.ResolveSession(r.Context(), token)
		if err != nil {
			s.clearSession(w)
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, sessionKey, sess)
		ctx = context.WithValue(ctx, tokenKey, token)
		next(w, r.WithContext(ctx))
	}
}
func (s *Server) withApproved(next http.HandlerFunc) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r).Status != domain.StatusApproved {
			http.Redirect(w, r, "/status", http.StatusSeeOther)
			return
		}
		next(w, r)
	})
}
func (s *Server) withAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.withUser(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if !u.IsAdmin() || u.Status != domain.StatusApproved {
			s.errorPage(w, r, http.StatusForbidden, "无权访问管理中心", nil)
			return
		}
		next(w, r)
	})
}
func currentUser(r *http.Request) domain.User {
	u, _ := r.Context().Value(userKey).(domain.User)
	return u
}
func currentSession(r *http.Request) store.Session {
	v, _ := r.Context().Value(sessionKey).(store.Session)
	return v
}
func currentToken(r *http.Request) string { v, _ := r.Context().Value(tokenKey).(string); return v }

func (s *Server) verifyCSRF(r *http.Request) bool {
	_ = r.ParseForm()
	provided := r.FormValue("csrf_token")
	if token := currentToken(r); token != "" {
		return security.VerifyCSRF(s.Secret, token, provided)
	}
	cookie, err := r.Cookie("cliproxy_pre_csrf")
	if err != nil {
		return false
	}
	return security.HMAC(s.Secret, "pre:"+cookie.Value) == provided
}
func (s *Server) preCSRF(w http.ResponseWriter, r *http.Request) string {
	raw := ""
	if c, err := r.Cookie("cliproxy_pre_csrf"); err == nil {
		raw = c.Value
	}
	if raw == "" {
		raw, _ = security.RandomToken(24)
		http.SetCookie(w, &http.Cookie{Name: "cliproxy_pre_csrf", Value: raw, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 3600})
	}
	return security.HMAC(s.Secret, "pre:"+raw)
}
func (s *Server) sessionToken(r *http.Request) string {
	c, err := r.Cookie(s.Cfg.CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
func (s *Server) setSession(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: s.Cfg.CookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: s.Cfg.CookieName, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func (s *Server) clientIP(r *http.Request) string {
	if s.Cfg.TrustProxyHeaders {
		if v := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.Logger.Info("http request", "method", r.Method, "path", r.URL.Path, "ip", s.clientIP(r), "duration_ms", time.Since(start).Milliseconds())
	})
}
func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Logger.Error("panic recovered", "error", fmt.Sprint(v))
				s.errorPage(w, r, 500, "页面暂时不可用", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		http.Error(w, "unhealthy", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}
