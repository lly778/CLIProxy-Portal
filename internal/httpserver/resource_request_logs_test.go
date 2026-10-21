package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/domain"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func TestProtectionRequestLogsPreserveOldEventsScopeAndActualStatuses(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st, Cfg: config.Config{TimeZone: time.UTC}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	for i, id := range []string{"a", "b"} {
		if err := st.CreateUser(t.Context(), domain.User{ID: id, Phone: []string{"13800138001", "13800138002"}[i], Name: "用户" + id, Role: domain.RoleUser, Status: domain.StatusApproved, CreatedAt: start, UpdatedAt: start}); err != nil {
			t.Fatal(err)
		}
	}
	// An already persisted event must move to request-log presentation too.
	if err := st.Audit(t.Context(), domain.AuditEvent{Action: "resource.rejected", TargetID: "a", TargetLabel: "用户a", Detail: "整机内存达到总上限", CreatedAt: start.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.resourceAudit(t.Context(), []resourcecontrol.Event{
		{At: start.Add(2 * time.Minute), User: "a", Reason: "user_concurrency"},
		{At: start.Add(3 * time.Minute), User: "a", Reason: "request_too_large_or_invalid", StatusCode: 400},
		{At: start.Add(4 * time.Minute), User: "b", Reason: "request_too_large_or_invalid", StatusCode: 413},
		{At: start.Add(5 * time.Minute), User: "a", Reason: "cpu_pressure", Observed: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Audit(t.Context(), domain.AuditEvent{Action: "resource.policy.update", TargetID: "a", Detail: "保留设置修改操作", CreatedAt: start}); err != nil {
		t.Fatal(err)
	}
	upstream := cpamp.EventsResponse{Items: []cpamp.EventRow{{RequestID: "real", TimestampMS: start.UnixMilli(), Model: "real-model", TotalTokens: 99}}, TotalCount: 10}
	page := s.supplementGatewayFailures(t.Context(), upstream, start, start.Add(time.Hour), "a", 100)
	if len(page.Items) != 5 || page.TotalCount != 14 || len(upstream.Items) != 1 || page.Items[0].PortalResourceID == 0 {
		t.Fatalf("scope/order/count or cached upstream mutation: %+v", page)
	}
	for i, code := range []int{0, 400, 429, 503} {
		e := page.Items[i]
		if e.PortalUserID != "a" || e.PortalUserLabel != "用户a" || e.RequestID != "" {
			t.Fatalf("wrong ownership or invented CPA ID: %+v", e)
		}
		view := s.requestView(e)
		if view.Model != "—" || view.InputTokens != "—" || view.TotalTokens != "—" || view.TotalLatency != "—" || view.Latency != "—" || view.CaptureID != "" {
			t.Fatalf("resource event invented model usage: %+v", view)
		}
		if i == 0 {
			if view.StatusLabel != "观察放行" || e.FailStatusCode != nil || view.Error != "Server CPU limit exceeded" {
				t.Fatalf("observed request incorrectly marked failed: %+v", view)
			}
		} else if view.StatusLabel != "已拦截" || e.FailStatusCode == nil || int(*e.FailStatusCode) != code || !strings.Contains(view.Error, "HTTP ") {
			t.Fatalf("wrong actual rejection status %d: %+v %+v", code, e, view)
		}
	}
	global := s.supplementGatewayFailures(t.Context(), upstream, start, start.Add(time.Hour), "", 2)
	if global.TotalCount != 15 || len(global.Items) != 2 || global.Items[1].PortalUserID != "b" {
		t.Fatalf("global limit or ownership: %+v", global)
	}
	views := s.globalRequestViews(t.Context(), global.Items)
	if len(views) != 2 || !views[0].Linked || views[0].User.ID != "a" || !views[1].Linked || views[1].User.ID != "b" || views[1].StatusLabel != "已拦截" {
		t.Fatalf("global request cards lost protection-event ownership or status: %+v", views)
	}
	for _, rows := range []func() ([]domain.AuditEvent, error){
		func() ([]domain.AuditEvent, error) { return st.ListAudit(t.Context(), 1, 0) },
		func() ([]domain.AuditEvent, error) { return st.ListAuditForUser(t.Context(), "a", 1, 0) },
	} {
		logs, err := rows()
		if err != nil || len(logs) != 1 || logs[0].Action != "resource.policy.update" {
			t.Fatalf("system logs did not filter protection events before limiting: %+v %v", logs, err)
		}
	}
	counts, err := st.ResourceCounts(t.Context(), time.Now())
	if err != nil || counts.Rejected != 4 || counts.Observed != 1 {
		t.Fatalf("moving logs changed seven-day counters: %+v %v", counts, err)
	}
	a := cpamp.AnalyticsResponse{Summary: &cpamp.UsageSummary{TotalCalls: 10, TotalTokens: 99}, Events: &upstream}
	a = s.usageWithGatewayFailures(t.Context(), a, start, start.Add(time.Hour), "a", 100)
	if a.Summary.TotalCalls != 10 || a.Summary.TotalTokens != 99 {
		t.Fatal("protection events changed model usage accounting")
	}
	renderer, err := webui.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	view := s.requestView(page.Items[1])
	for name, data := range map[string]any{
		webui.PageActivity:      webui.ActivityView{Requests: []webui.RequestView{view}},
		webui.PageAdminRequests: webui.AdminRequestsView{Requests: []webui.GlobalRequestView{{RequestView: view, UserLabel: "用户a"}}},
	} {
		var out bytes.Buffer
		if err := renderer.Execute(&out, name, data); err != nil || !strings.Contains(out.String(), "已拦截") || !strings.Contains(out.String(), "HTTP 400") {
			t.Fatalf("missing protection request row on %s: %v", name, err)
		}
	}
}

func TestRemoteUsageCannotInjectLocalProtectionMetadata(t *testing.T) {
	var row cpamp.EventRow
	if err := json.Unmarshal([]byte(`{"PortalResourceID":1,"PortalUserID":"admin","PortalUserLabel":"fake","PortalResourceObserved":true}`), &row); err != nil {
		t.Fatal(err)
	}
	if row.PortalResourceID != 0 || row.PortalUserID != "" || row.PortalUserLabel != "" || row.PortalResourceObserved {
		t.Fatal("remote CPAMP JSON injected privileged local metadata")
	}
}
