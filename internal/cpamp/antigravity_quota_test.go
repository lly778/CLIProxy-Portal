package cpamp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAntigravityQuotaProxyFallback(t *testing.T) {
	calls := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != pathAPICall {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body struct {
			AuthIndex, Method, URL, Data string
			Header                       map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.AuthIndex != "anti-index" || body.Method != "POST" || body.Header["Authorization"] != "Bearer $TOKEN$" || body.Data != `{"project":"project-id"}` {
			t.Errorf("invalid proxy query %#v", body)
		}
		if strings.Contains(body.URL, "retrieveUserQuotaSummary") {
			_, _ = w.Write([]byte(`{"status_code":403,"body":{"error":"unavailable"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"statusCode":200,"body":"{\"models\":{\"claude-model\":{\"quotaInfo\":{\"remainingFraction\":0.4,\"resetTime\":\"2026-10-04T18:00:00Z\"}},\"gemini-model\":{\"quotaInfo\":{\"remainingFraction\":0.9}}}}"}`))
	})
	windows, err := client.FetchAntigravityQuota(t.Context(), AuthFile{AuthIndex: "anti-index", ProjectID: "project-id"}, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if err != nil || len(windows) != 2 || windows[0].RemainingPercent != 40 || windows[1].RemainingPercent != 90 || calls != 4 {
		t.Fatalf("windows=%#v calls=%d err=%v", windows, calls, err)
	}
}

func TestAntigravityQuotaGroupingAndUnknowns(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal([]byte(`{"tabModelIds":["gemini-tab"],"models":{"claude-one":{"quotaInfo":{"remainingFraction":0.3,"resetTime":"2026-10-04T18:00:00Z"}},"claude-two":{"quotaInfo":{"remainingFraction":0.3,"resetTime":"2026-10-04T19:00:00Z"}},"gemini-pro":{"quotaInfo":{"remainingFraction":1.4}},"gemini-tab":{"quotaInfo":{"remainingFraction":0}},"gemini-unknown":{"quotaInfo":{"resetTime":"2026-10-04T18:00:00Z"}}}}`), &payload)
	w := buildAntigravityQuotaWindows(payload, time.Now())
	if len(w) != 2 || w[0].RemainingPercent != 30 || w[0].ResetAt.Hour() != 19 || w[1].RemainingPercent != 100 {
		t.Fatalf("groups %#v", w)
	}
	_ = json.Unmarshal([]byte(`{"groups":[{"displayName":"Gemini","buckets":[{"bucketId":"one","displayName":"Daily","remainingFraction":0.25},{"bucketId":"unknown"}]}]}`), &payload)
	w = buildAntigravityQuotaWindows(payload, time.Now())
	if len(w) != 1 || w[0].Label != "Gemini · Daily" || w[0].RemainingPercent != 25 {
		t.Fatalf("native windows %#v", w)
	}
}

func TestAntigravityQuotaRateLimitStopsFallback(t *testing.T) {
	calls := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"status_code":429,"body":{"error":"quota"}}`))
	})
	_, err := client.FetchAntigravityQuota(t.Context(), AuthFile{AuthIndex: "anti-index"}, time.Now())
	if err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
