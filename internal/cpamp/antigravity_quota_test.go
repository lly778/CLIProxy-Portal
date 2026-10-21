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

func TestAntigravityHubClientReturnsNativeGeminiFiveHourAndWeeklyBuckets(t *testing.T) {
	calls := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var query struct {
			URL    string
			Header map[string]string
		}
		_ = json.NewDecoder(r.Body).Decode(&query)
		if query.Header["User-Agent"] != "antigravity/hub/2.23.0 darwin/arm64" {
			t.Error("outdated client would omit Gemini native periods")
		}
		if !strings.Contains(query.URL, "retrieveUserQuotaSummary") {
			t.Error("complete native quota must not use model fallback")
		}
		_, _ = w.Write([]byte(`{"status_code":200,"body":{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","displayName":"Five Hour Limit Remaining","window":"5h","remainingFraction":0.42,"resetTime":"2026-10-22T04:00:00Z"},{"bucketId":"gemini-weekly","displayName":"Weekly Limit Remaining","window":"weekly","remainingFraction":0.73,"resetTime":"2026-10-28T04:00:00Z"}]},{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","remainingFraction":0.91},{"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","remainingFraction":0.95}]}]}}`))
	})
	windows, err := client.FetchAntigravityQuota(t.Context(), AuthFile{AuthIndex: "anti-index"}, time.Now())
	if err != nil || len(windows) != 4 || calls != 1 {
		t.Fatal("native periods lost", windows, calls, err)
	}
	var gemini []AntigravityQuotaWindow
	for _, window := range windows {
		if antigravityFamily(window) == "gemini" {
			gemini = append(gemini, window)
		}
	}
	if len(gemini) != 2 || gemini[0].RemainingPercent != 42 || gemini[1].RemainingPercent != 73 || gemini[0].ResetAt.IsZero() || gemini[1].ResetAt.IsZero() {
		t.Fatal("Gemini periods combined or lost reset time", gemini)
	}
}

func TestAntigravityQuotaGroupingAndUnknowns(t *testing.T) {
	var payload map[string]any
	_ = json.Unmarshal([]byte(`{"tabModelIds":["gemini-tab"],"models":{"claude-one":{"quotaInfo":{"remainingFraction":0.3,"resetTime":"2026-10-04T18:00:00Z"}},"claude-two":{"quotaInfo":{"remainingFraction":0.3,"resetTime":"2026-10-04T19:00:00Z"}},"gemini-pro":{"quotaInfo":{"remainingFraction":1.4}},"gemini-tab":{"quotaInfo":{"remainingFraction":0}},"gemini-unknown":{"quotaInfo":{"resetTime":"2026-10-04T18:00:00Z"}}}}`), &payload)
	w := buildAntigravityQuotaWindows(payload, time.Now())
	if len(w) != 2 || w[0].RemainingPercent != 30 || w[0].ResetAt.Hour() != 19 || w[1].RemainingPercent != 100 {
		t.Fatalf("groups %#v", w)
	}
	payload = nil
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

func TestAntigravityPartialSummaryAddsMissingGeminiWithoutReplacingNativePeriods(t *testing.T) {
	for _, rateLimited := range []bool{false, true} {
		calls := 0
		client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			var query struct{ URL string }
			_ = json.NewDecoder(r.Body).Decode(&query)
			if strings.Contains(query.URL, "retrieveUserQuotaSummary") {
				_, _ = w.Write([]byte(`{"status_code":200,"body":{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","displayName":"Five Hour Limit Remaining","remainingFraction":0.4},{"bucketId":"3p-weekly","displayName":"Weekly Limit Remaining","remainingFraction":0.8}]}]}}`))
			} else if rateLimited {
				_, _ = w.Write([]byte(`{"status_code":429,"body":{}}`))
			} else {
				_, _ = w.Write([]byte(`{"status_code":200,"body":{"models":{"claude-model":{"quotaInfo":{"remainingFraction":1}},"gemini-model":{"quotaInfo":{"remainingFraction":0.9}}}}}`))
			}
		})
		windows, err := client.FetchAntigravityQuota(t.Context(), AuthFile{AuthIndex: "anti-index"}, time.Now())
		want := 3
		if rateLimited {
			want = 2
		}
		if err != nil || len(windows) != want || calls != 2 || windows[0].RemainingPercent != 40 || windows[1].RemainingPercent != 80 {
			t.Fatalf("partial summary lost or replaced: windows=%+v calls=%d err=%v", windows, calls, err)
		}
		if !rateLimited && (windows[2].RemainingPercent != 90 || windows[2].Label != "Gemini · 模型额度") {
			t.Fatal("Gemini quota missing or its period invented", windows[2])
		}
	}
}

func TestAntigravityCompleteSummaryAvoidsCatalogAndMixedPayloadKeepsBothFamilies(t *testing.T) {
	payload := `{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p","remainingFraction":0.4}]},{"displayName":"Gemini models","buckets":[{"bucketId":"gemini","remainingFraction":0.9}]}]}`
	calls := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"status_code":200,"body":` + payload + `}`))
	})
	windows, err := client.FetchAntigravityQuota(t.Context(), AuthFile{AuthIndex: "anti-index"}, time.Now())
	if err != nil || calls != 1 || len(windows) != 2 {
		t.Fatal("complete summary caused redundant request", windows, calls, err)
	}
	var mixed map[string]any
	_ = json.Unmarshal([]byte(`{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p","remainingFraction":0.4}]}],"models":{"gemini-model":{"quotaInfo":{"remainingFraction":0.9}}}}`), &mixed)
	windows = buildAntigravityQuotaWindows(mixed, time.Now())
	if len(windows) != 2 || !hasAntigravityFamily(windows, "gemini") || !hasAntigravityFamily(windows, "claude-gpt") {
		t.Fatal("mixed response dropped family", windows)
	}
}
