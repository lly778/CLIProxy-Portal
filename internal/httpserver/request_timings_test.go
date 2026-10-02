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
	for _, want := range []string{"服务器耗时：473000 ms", "三段相加为服务器耗时", "接入及请求上传：465000 ms", "请求收齐到开始写出回复：5000 ms", "持续生成及发送回复：3000 ms", "其中：首 token 等待 5000 ms"} {
		if !strings.Contains(v.LatencyDetail, want) {
			t.Errorf("detail missing %q: %s", want, v.LatencyDetail)
		}
	}
	if strings.Contains(v.LatencyDetail, "总耗时") {
		t.Fatal("tooltip should consistently use 耗时")
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
