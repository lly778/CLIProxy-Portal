package httpserver

import (
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/webui"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func resourceReason(reason string) string {
	switch reason {
	case "global_concurrency":
		return "全局并发已满"
	case "user_concurrency":
		return "用户并发已满"
	case "capture_backlog":
		return "交互记录待处理过多"
	case "memory_pressure":
		return "整机内存达到总上限"
	case "cpu_pressure":
		return "CPU 持续繁忙"
	case "monitor_unavailable":
		return "资源监测未连接"
	case "request_too_large", "websocket_message_too_large":
		return "请求超过大小上限"
	case "request_too_large_or_invalid":
		return "请求过大或内容无效"
	}
	return "正常"
}

func (s *Server) resourceAudit(ctx context.Context, events []resourcecontrol.Event) error {
	var rows []domain.AuditEvent
	for _, e := range events {
		action := "resource.rejected"
		if e.Observed {
			action = "resource.observed"
		}
		id, name := "", e.User
		if e.User == "unassigned" {
			name = "非门户 Key"
		} else if e.User == "" {
			name = "多个用户"
		} else if u, err := s.Store.UserByID(ctx, e.User); err == nil {
			id, name = u.ID, u.Name
		}
		rows = append(rows, domain.AuditEvent{ActorLabel: "资源保护", Action: action, TargetID: id, TargetLabel: name, Detail: resourceReason(e.Reason), CreatedAt: e.At})
	}
	err := s.Store.ResourceAuditBatch(ctx, rows)
	if err != nil {
		s.Logger.Warn("resource audit flush failed", "error", err)
	}
	return err
}
func (s *Server) resourceView(ctx context.Context) webui.ResourceView {
	q := s.Resources.Snapshot()
	v := webui.ResourceView{Policy: q.Policy, Mode: "观察模式", State: "正常", StateClass: "success", CPU: "—", Memory: "—", Available: "等待监测", Active: fmt.Sprintf("%d / %d", q.Active, q.Policy.Global), Processing: strconv.Itoa(q.Processing), Rejected: "—", Observed: "—", Fresh: q.Fresh, Updated: "等待监测"}
	countsCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if counts, err := s.Store.ResourceCounts(countsCtx, time.Now()); err == nil {
		v.Rejected = strconv.FormatUint(counts.Rejected, 10)
		v.Observed = strconv.FormatUint(counts.Observed, 10)
	}
	if q.Policy.Global == 0 {
		v.Active = fmt.Sprintf("%d / 不限", q.Active)
	}
	if q.Policy.Mode == "enforce" {
		v.Mode = "保护已启用"
	}
	if q.Paused != "" {
		v.State = resourceReason(q.Paused)
		v.StateClass = "danger"
		v.StateHelp = "当前为观察模式，请求仍会继续放行。"
		if q.Policy.Mode == "enforce" {
			v.State = "暂停新请求 · " + v.State
			v.StateHelp = "已有请求继续处理，资源恢复稳定后自动接收新请求。"
		}
	}
	if q.Fresh {
		v.CPU = fmt.Sprintf("%.1f%%", q.Metrics.CPUPercent)
		v.Memory = fmt.Sprintf("%.0f / %.0f MiB", q.Metrics.UsedMiB(), q.Metrics.TotalMiB)
		v.Available = fmt.Sprintf("可用 %.0f MiB", q.Metrics.AvailableMiB)
		v.Updated = s.formatTime(q.Metrics.At)
		if q.Policy.MemoryMiB > 0 && float64(q.Policy.MemoryMiB) > q.Metrics.TotalMiB-64 {
			v.Warning = "内存总上限过高，请至少保留 64 MiB 余量"
		}
	}
	return v
}

func (s *Server) resourceStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.resourceView(r.Context()))
}

func (s *Server) resourceHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	now := time.Now().UTC()
	zone := "Asia/Shanghai"
	if s.Cfg.TimeZone != nil {
		zone = s.Cfg.TimeZone.String()
	}
	_ = json.NewEncoder(w).Encode(struct {
		From          time.Time                      `json:"from"`
		To            time.Time                      `json:"to"`
		TimeZone      string                         `json:"time_zone"`
		SampleSeconds int                            `json:"sample_seconds"`
		Points        []resourcecontrol.HistoryPoint `json:"points"`
	}{now.Add(-resourcecontrol.HistoryWindow), now, zone, int(resourcecontrol.HistoryInterval / time.Second), s.Resources.History(now)})
}
func (s *Server) resourceAuthorize(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil || !s.verifyCSRF(r) {
		s.errorPage(w, r, 403, "请求已失效，请重试", nil)
		return false
	}
	for _, values := range r.PostForm {
		if len(values) != 1 {
			s.errorPage(w, r, 400, "资源设置包含重复字段", nil)
			return false
		}
	}
	return true
}
func (s *Server) resourceRedirect(w http.ResponseWriter, r *http.Request, kind, message string) {
	http.Redirect(w, r, "/admin/system?resource_"+kind+"="+url.QueryEscape(message)+"#resources", http.StatusSeeOther)
}
func (s *Server) resourceSave(w http.ResponseWriter, r *http.Request) {
	if !s.resourceAuthorize(w, r) {
		return
	}
	bad := false
	integer := func(name string) int64 {
		n, e := strconv.ParseInt(r.PostForm.Get(name), 10, 64)
		if e != nil {
			bad = true
		}
		return n
	}
	decimal := func(name string) float64 {
		n, e := strconv.ParseFloat(r.PostForm.Get(name), 64)
		if e != nil {
			bad = true
		}
		return n
	}
	p := resourcecontrol.Policy{Revision: integer("revision"), Mode: r.PostForm.Get("mode"), Global: int(integer("global")), PerUser: int(integer("per_user")), BodyMiB: integer("body_mib"), CPUPercent: decimal("cpu_percent"), MemoryMiB: integer("memory_mib")}
	if bad {
		s.resourceRedirect(w, r, "error", "设置格式无效")
		return
	}
	if err := p.Validate(); err != nil {
		s.resourceRedirect(w, r, "error", err.Error())
		return
	}
	m := s.Resources.Snapshot().Metrics
	if p.MemoryMiB > 0 && m.Fresh(time.Now()) && float64(p.MemoryMiB) > m.TotalMiB-64 {
		s.resourceRedirect(w, r, "error", "总内存上限过高，请至少保留 64 MiB 余量")
		return
	}
	if err := s.Store.SaveResourcePolicy(r.Context(), p); err != nil {
		message := "保存失败，请刷新后重试"
		if strings.Contains(err.Error(), "其他管理员") {
			message = err.Error()
		}
		s.resourceRedirect(w, r, "error", message)
		return
	}
	saved, err := s.Store.ResourcePolicy(r.Context())
	if err != nil {
		s.resourceRedirect(w, r, "error", "设置已保存，但暂时无法读取，请刷新确认")
		return
	}
	s.Resources.SetPolicy(saved)
	mode := "观察模式"
	if saved.Mode == "enforce" {
		mode = "启用保护"
	}
	s.audit(r, currentUser(r), "resource.policy.update", "资源保护", fmt.Sprintf("模式=%s；全局并发=%d；每用户并发=%d；请求上限=%d MiB；CPU 总上限=%.1f%%；内存总上限=%d MiB；版本=%d", mode, saved.Global, saved.PerUser, saved.BodyMiB, saved.CPUPercent, saved.MemoryMiB, saved.Revision))
	s.resourceRedirect(w, r, "msg", "资源保护设置已保存，对后续请求生效")
}
