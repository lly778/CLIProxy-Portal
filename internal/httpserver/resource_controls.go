package httpserver

import (
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/webui"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Convert at the HTTP boundary; persisted metrics and policy retain their unit.
func resourceMB(mib float64) float64 {
	return math.Round(mib*float64(resourcecontrol.MiB)) / 1_000_000
}

type resourceHistoryPoint struct {
	At         time.Time `json:"at"`
	CPUPercent float64   `json:"cpu_percent"`
	UsedMB     float64   `json:"used_mb"`
	TotalMB    float64   `json:"total_mb"`
}

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
		status := e.StatusCode
		if status == 0 && !e.Observed {
			status = resourceRejectionStatus(e.Reason)
		}
		if e.Observed {
			status = 0
		}
		detail, err := json.Marshal(resourceRequestDetail{Reason: e.Reason, Message: resourceReason(e.Reason), StatusCode: status})
		if err != nil {
			return err
		}
		rows = append(rows, domain.AuditEvent{ActorLabel: "资源保护", Action: action, TargetID: id, TargetLabel: name, Detail: string(detail), CreatedAt: e.At})
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
	v.MemoryMB = q.Policy.MemoryMB
	v.ESXi = s.Cfg.HostMetricsPath != ""
	v.AGYConfigured = s.Keys != nil && s.Keys.AGY != nil
	if v.AGYConfigured {
		v.AGYTasks = "其中：AGY 任务 —"
		v.AGYResident = "—"
		if status, err := s.Keys.AGY.Status(ctx); err == nil {
			v.AGYTasks = fmt.Sprintf("其中：AGY 任务 %d", status.ActiveSessions)
			v.AGYResident = fmt.Sprintf("%d / %d", status.Sessions, status.MaxSessions)
		}
	}
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
		v.Memory = fmt.Sprintf("%.0f MB / 不限", q.Metrics.UsedMB())
		if q.Policy.MemoryMB > 0 {
			v.Memory = fmt.Sprintf("%.0f / %.0f MB", q.Metrics.UsedMB(), v.MemoryMB)
		}
		v.Available = fmt.Sprintf("整机可用 %.0f / %.0f MB", q.Metrics.AvailableMB(), q.Metrics.TotalMB())
		if v.ESXi && q.Metrics.HostUsedMiB != nil {
			v.Available = fmt.Sprintf("宿主机可用 %.0f / %.0f MB", resourceMB(q.Metrics.HostTotalMiB-*q.Metrics.HostUsedMiB), resourceMB(q.Metrics.HostTotalMiB))
		}
		v.Updated = s.formatTime(q.Metrics.At)
		if q.Policy.MemoryMB > 0 && v.MemoryMB > q.Metrics.TotalMB()-64 {
			v.Warning = "内存总上限过高，请至少保留 64 MB 余量"
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
	history := s.Resources.History(now)
	points := make([]resourceHistoryPoint, 0, len(history))
	for _, p := range history {
		points = append(points, resourceHistoryPoint{At: p.At, CPUPercent: p.CPUPercent, UsedMB: resourceMB(p.UsedMiB), TotalMB: resourceMB(p.TotalMiB)})
	}
	_ = json.NewEncoder(w).Encode(struct {
		From          time.Time              `json:"from"`
		To            time.Time              `json:"to"`
		TimeZone      string                 `json:"time_zone"`
		SampleSeconds int                    `json:"sample_seconds"`
		Points        []resourceHistoryPoint `json:"points"`
	}{now.Add(-resourcecontrol.HistoryWindow), now, zone, int(resourcecontrol.HistoryInterval / time.Second), points})
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
	// Old open tabs still submit MiB. New forms submit decimal MB, rounded only
	// to whole bytes so saving an unchanged setting preserves the exact limit.
	sizeMB := func(name string, maxMB float64) float64 {
		if _, ok := r.PostForm[name+"_mb"]; !ok {
			return resourceMB(float64(integer(name + "_mib")))
		}
		mb := decimal(name + "_mb")
		if math.IsNaN(mb) || math.IsInf(mb, 0) || mb < 0 || mb > maxMB || (mb > 0 && mb < 0.000001) {
			bad = true
			return 0
		}
		return math.Round(mb*1_000_000) / 1_000_000
	}
	residentLimit := s.Resources.Policy().AGYResidentLimit
	if _, ok := r.PostForm["agy_resident_limit"]; ok {
		residentLimit = int(integer("agy_resident_limit"))
	}
	p := resourcecontrol.Policy{Revision: integer("revision"), Mode: r.PostForm.Get("mode"), Global: int(integer("global")), PerUser: int(integer("per_user")), AGYResidentLimit: residentLimit, CPUPercent: decimal("cpu_percent"), MemoryMB: sizeMB("memory", 1099511.627776)}
	if bad {
		s.resourceRedirect(w, r, "error", "设置格式无效")
		return
	}
	if err := p.Validate(); err != nil {
		s.resourceRedirect(w, r, "error", err.Error())
		return
	}
	m := s.Resources.Snapshot().Metrics
	if p.MemoryMB > 0 && m.Fresh(time.Now()) && p.MemoryMB > m.TotalMB()-64 {
		s.resourceRedirect(w, r, "error", "总内存上限过高，请至少保留 64 MB 余量")
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
	s.audit(r, currentUser(r), "resource.policy.update", "资源保护", fmt.Sprintf("模式=%s；全局并发=%d；每用户并发=%d；整机 CPU 上限=%.1f%%；整机内存上限=%g MB；AGY 常驻对话上限=%d", mode, saved.Global, saved.PerUser, saved.CPUPercent, saved.MemoryMB, saved.AGYResidentLimit))
	if s.Keys != nil && s.Keys.AGY != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		err := s.Keys.AGY.SetResidentLimit(ctx, saved.AGYResidentLimit, saved.Revision)
		cancel()
		if err != nil {
			s.resourceRedirect(w, r, "error", "设置已保存，但 AGY 常驻对话上限暂未完全应用，请重试")
			return
		}
	}
	s.resourceRedirect(w, r, "msg", "资源保护设置已保存，对后续请求生效")
}
