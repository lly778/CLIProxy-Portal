package httpserver

import (
	"bytes"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func TestGatewayFailureLogsAreScopedSortedAndDoNotInventModelUsage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st, Cfg: config.Config{TimeZone: time.UTC}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)
	write := int64(117502)
	for _, row := range []store.GatewayRequestTiming{
		{ID: "failure", UserID: "a", APIKeyHash: "hash-a", StartedAt: start.Add(time.Minute), TotalMS: 118003, ResponseStartedMS: &write, StatusCode: 502, FailureReason: "upload_interrupted"},
		{ID: "other-user", UserID: "b", APIKeyHash: "hash-b", StartedAt: start.Add(2 * time.Minute), TotalMS: 118003, ResponseStartedMS: &write, FailureReason: "upload_interrupted"},
		{ID: "outside-range", UserID: "a", StartedAt: start.Add(-time.Minute), TotalMS: 118003, ResponseStartedMS: &write, FailureReason: "upload_interrupted"},
		{ID: "already-in-model-log", UserID: "a", CPARequestID: "model-id", StartedAt: start.Add(3 * time.Minute), TotalMS: 118003, FailureReason: "upstream_request_failed"},
		{ID: "not-a-failure", UserID: "a", StartedAt: start.Add(4 * time.Minute), TotalMS: 118003, Completed: true},
	} {
		row.EndedAt = row.StartedAt.Add(time.Duration(row.TotalMS) * time.Millisecond)
		if err := st.SaveGatewayRequestTiming(t.Context(), row); err != nil {
			t.Fatal(err)
		}
	}
	page := cpamp.EventsResponse{Items: []cpamp.EventRow{{RequestID: "model-id", TimestampMS: start.UnixMilli(), Model: "test", TotalTokens: 99}}, TotalCount: 15}
	merged := s.supplementGatewayFailures(t.Context(), page, start, start.Add(time.Hour), "a", 100)
	if len(merged.Items) != 2 || merged.TotalCount != 16 || merged.Items[0].PortalTimingID != "failure" || merged.Items[1].RequestID != "model-id" {
		t.Fatalf("scope/order/count: %+v", merged)
	}
	if len(page.Items) != 1 || page.Items[0].PortalTimingID != "" {
		t.Fatal("upstream cached page mutated")
	}
	view := s.requestView(merged.Items[0])
	for _, want := range []string{"接入及请求上传：117502 ms", "请求收齐到开始写出回复：未进入（0 ms）", "错误回复及连接收尾：501 ms", "模型调用耗时：未进入"} {
		if !strings.Contains(view.LatencyDetail, want) {
			t.Errorf("missing %q in %s", want, view.LatencyDetail)
		}
	}
	if view.TotalLatency != "118003 ms" || view.Model != "—" || view.TotalTokens != "—" || view.InputTokens != "—" || view.Status != "failed" || !strings.Contains(view.Error, "Request upload incomplete") {
		t.Fatalf("fabricated/lost metadata: %+v", view)
	}
	renderer, err := webui.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	for page, data := range map[string]any{
		webui.PageActivity:      webui.ActivityView{Requests: []webui.RequestView{view}},
		webui.PageAdminRequests: webui.AdminRequestsView{Requests: []webui.GlobalRequestView{{RequestView: view, UserLabel: "农玉雪"}}},
	} {
		var html bytes.Buffer
		if err := renderer.Execute(&html, page, data); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"118003 ms", "Request upload incomplete", "接入及请求上传：117502 ms", "错误回复及连接收尾：501 ms"} {
			if !strings.Contains(html.String(), want) {
				t.Errorf("%s missing %q", page, want)
			}
		}
	}
	global := s.supplementGatewayFailures(t.Context(), page, start, start.Add(time.Hour), "", 1)
	if len(global.Items) != 1 || global.Items[0].PortalTimingID != "other-user" || global.TotalCount != 17 {
		t.Fatalf("global ordering/limit: %+v", global)
	}
}
