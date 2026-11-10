package httpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

func TestGatewayIdleTimeoutShowsFailureDespiteUpstreamSuccess(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	read, firstWrite := int64(100), int64(1000)
	row := store.GatewayRequestTiming{ID: "idle", UserID: "owner", APIKeyHash: "key", CPARequestID: "request", StartedAt: now, EndedAt: now.Add(61 * time.Second), TotalMS: 61000, RequestReadMS: &read, ResponseStartedMS: &firstWrite, StatusCode: 504, FailureReason: "idle_timeout"}
	if err := st.SaveGatewayRequestTiming(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Cfg: config.Config{TimeZone: time.UTC}}
	event := cpamp.EventRow{RequestID: "request", APIKeyHash: "key", TimestampMS: now.Add(time.Second).UnixMilli(), InputTokens: 10, OutputTokens: 20, TotalTokens: 30}
	view := s.requestView(event)
	if view.Status != "failed" || !strings.Contains(view.ErrorFull, "idle timeout") || !strings.Contains(view.LatencyDetail, "连续 60 秒") || view.TotalTokens != "30" {
		t.Fatalf("upstream success concealed gateway timeout or lost usage: %+v", view)
	}
}

func TestTotalTimingRequiresExactRequestAndKeyAndDoesNotInventOldTotals(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Store: st, Cfg: config.Config{TimeZone: time.UTC}}
	now := time.Now().UTC()
	model, ttft := int64(8000), int64(5000)
	e := cpamp.EventRow{RequestID: "abcd1234", APIKeyHash: "hash-a", TimestampMS: now.Add(465 * time.Second).UnixMilli(), LatencyMS: &model, TTFTMS: &ttft}
	if v := s.requestView(e); v.TotalLatency != "—" || !strings.Contains(v.LatencyDetail, "8000 ms") {
		t.Fatalf("old event invented a total: %+v", v)
	}
	read, headers, write := int64(465000), int64(467000), int64(470000)
	row := store.GatewayRequestTiming{ID: "timing-a", UserID: "owner", APIKeyHash: "hash-a", CPARequestID: e.RequestID, StartedAt: now, EndedAt: now.Add(473 * time.Second), TotalMS: 473000, RequestReadMS: &read, UpstreamHeadersMS: &headers, ResponseStartedMS: &write, Completed: true, StartSource: "connection_accepted"}
	if err := st.SaveGatewayRequestTiming(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	v := s.requestView(e)
	if v.TotalLatency != "473000 ms" || v.Latency != "8000 ms" {
		t.Fatalf("total/model duration mixed: %+v", v)
	}
	for _, want := range []string{"服务器耗时：473000 ms", "各阶段耗时（完整记录时，三段相加为总耗时）", "接入及请求上传：465000 ms", "请求收齐到开始写出回复：5000 ms", "持续生成及发送回复：3000 ms", "其中：首 token 等待 5000 ms"} {
		if !strings.Contains(v.LatencyDetail, want) {
			t.Errorf("detail missing %q: %s", want, v.LatencyDetail)
		}
	}
	if strings.Contains(v.LatencyDetail, "服务器总耗时") || strings.Count(v.LatencyDetail, "总耗时") != 1 {
		t.Fatal("only the stage explanation should use 总耗时")
	}
	e.APIKeyHash = "hash-b"
	if v := s.requestView(e); v.TotalLatency != "—" {
		t.Fatalf("other key received timing: %+v", v)
	}
	e.APIKeyHash = "hash-a"
	e.TimestampMS = now.Add(24 * time.Hour).UnixMilli()
	if v := s.requestView(e); v.TotalLatency != "—" {
		t.Fatalf("reused ID outside interval received timing: %+v", v)
	}
}

func TestIncompleteTimingShowsOnlyObservedStages(t *testing.T) {
	row := store.GatewayRequestTiming{TotalMS: 100, Completed: false, StartSource: "headers_received"}
	total, detail := requestTimingDisplay(row, cpamp.EventRow{})
	if total != "100 ms" || !strings.Contains(detail, "中断") || !strings.Contains(detail, "接入及请求上传：—") || strings.Contains(detail, "接入及请求上传：0 ms") {
		t.Fatalf("unobserved stages presented as measured: %s %s", total, detail)
	}
}

func TestAGYMeasuredUsageSurvivesPortalTimingAndUnknownKeepsEffort(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	timing := store.GatewayRequestTiming{ID: "agy-usage", UserID: "owner", APIKeyHash: "key", CPARequestID: "agy:agy-usage", StartedAt: now, EndedAt: now.Add(time.Second), TotalMS: 1000, Completed: true, StatusCode: 200}
	if err = st.SaveGatewayRequestTiming(t.Context(), timing); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Cfg: config.Config{TimeZone: time.UTC}}
	known := true
	e := cpamp.EventRow{PortalTimingID: timing.ID, PortalUsageKnown: &known, APIKeyHash: "key", TimestampMS: now.UnixMilli(), ReasoningEffort: "high", InputTokens: 310, OutputTokens: 40, CacheReadTokens: 210, ReasoningTokens: 30, TotalTokens: 350}
	view := s.requestView(e)
	if view.InputTokens != "310" || view.OutputTokens != "40" || view.CacheTokens != "210" || view.ReasoningTokens != "30" || view.TotalTokens != "350" || view.ReasoningEffort != reasoningEffortLabel("high") || view.TotalLatency != "1000 ms" {
		t.Fatalf("measured AGY fields hidden: %+v", view)
	}
	known = false
	view = s.requestView(e)
	if view.InputTokens != "—" || view.TotalTokens != "—" || view.ReasoningEffort != reasoningEffortLabel("high") {
		t.Fatalf("unknown usage invented / effort hidden: %+v", view)
	}
	e.PortalUsageKnown = nil
	view = s.requestView(e)
	if view.TotalTokens != "—" {
		t.Fatal("gateway-only failure invented model usage")
	}
}
