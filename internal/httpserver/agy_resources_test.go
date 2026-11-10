package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cliproxy-portal/internal/agy"
	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/resourcecontrol"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
	"cliproxy-portal/internal/webui"
)

func TestAGYResourcesShowNativeTasksWithoutAddingThemToGatewayCount(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "portal.db"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"active_sessions":2,"waiting_sessions":1,"sessions":3,"max_sessions":3}`))
	}))
	defer worker.Close()
	client, _ := agy.NewClient(worker.URL, strings.Repeat("s", 64))
	s := &Server{Store: st, Keys: &service.Keys{AGY: client}, Resources: resourcecontrol.NewController(resourcecontrol.DefaultPolicy(), ""), Cfg: config.Config{TimeZone: time.UTC}}
	v := s.resourceView(t.Context())
	if !v.AGYConfigured || v.AGYTasks != "其中：AGY 任务 2" || v.AGYResident != "3 / 3" || !strings.HasPrefix(v.Active, "0 /") {
		t.Fatalf("task counters mixed with request count: %+v", v)
	}
	worker.Close()
	failed, _ := agy.NewClient(worker.URL, strings.Repeat("s", 64))
	s.Keys.AGY = failed
	if v = s.resourceView(t.Context()); !strings.Contains(v.AGYTasks, "—") {
		t.Fatal("unavailable worker displayed false zero tasks")
	}
}

func TestAGYStorageJoinsOtherStorageAndMissingMetricsStayUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	client, _ := agy.NewClient("http://127.0.0.1", strings.Repeat("s", 64))
	s := &Server{Keys: &service.Keys{AGY: client}, Cfg: config.Config{HostLogMetricsPath: path, TimeZone: time.UTC}}
	write := func(extra map[string]any) {
		t.Helper()
		v := map[string]any{"generated_at": time.Now().UTC(), "cpa_main_log_bytes": 100, "cpa_response_log_bytes": 200, "container_log_bytes": map[string]int64{"portal": 10, "cpa": 20, "cpamp": 30}}
		for k, x := range extra {
			v[k] = x
		}
		b, _ := json.Marshal(v)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"agy_data_bytes": 1024})
	views := s.hostLogHealthViews()
	if len(views) != 4 || views[3].StorageBytes != 1024 {
		t.Fatalf("AGY bytes not collected: %+v", views)
	}
	views = append([]webui.HealthCheckView{{Component: "门户 SQLite"}, {Component: "CPAMP SQLite"}}, views...)
	result := compactStorageViews(views)
	if len(result) != 3 || result[2].StorageBytes != 1384 || result[2].Message != "交互记录、AGY 数据与日志文件合计" {
		t.Fatalf("AGY not added exactly once: %+v", result)
	}
	write(nil)
	result = compactStorageViews(s.hostLogHealthViews())
	if result[0].Metric != "—" || result[0].Status != "warning" {
		t.Fatal("old collector missing AGY falsely showed complete total")
	}
	write(map[string]any{"agy_data_bytes": -1})
	if _, err := readHostLogMetrics(path, time.Now()); err == nil {
		t.Fatal("negative storage accepted")
	}
}

func TestAGYModelUpstreamHealthCombinesSourcesWithoutGeneration(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		workerDown, loggedOut, cpaDown bool
		status, label                  string
	}{
		{"both", false, false, false, "healthy", "服务可用"},
		{"worker-down", true, false, false, "warning", "部分可用"},
		{"logged-out", false, true, false, "warning", "部分可用"},
		{"cpa-down", false, false, true, "warning", "部分可用"},
		{"both-down", true, false, true, "error", "需检查"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/models" {
					t.Error("bad CPA probe")
				}
				if tc.cpaDown {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(401)
				}
			}))
			defer cpa.Close()
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/status":
					if tc.workerDown {
						w.WriteHeader(503)
					} else {
						w.Write([]byte(`{"ready":true}`))
					}
				case "/internal/account":
					if tc.loggedOut {
						w.Write([]byte(`{"logged_in":false}`))
					} else {
						w.Write([]byte(`{"logged_in":true}`))
					}
				default:
					t.Error("health triggered model / quota operation")
					w.WriteHeader(500)
				}
			}))
			defer worker.Close()
			client, _ := agy.NewClient(worker.URL, strings.Repeat("s", 64))
			s := &Server{Cfg: config.Config{CPAUpstreamURL: cpa.URL, TimeZone: time.UTC}, Keys: service.NewKeys(nil, nil)}
			s.Keys.AGY = client
			v := s.modelUpstreamHealthView(t.Context())
			if v.Component != "模型上游" || v.Status != tc.status || v.Metric != tc.label || !strings.Contains(v.Message, "CPA") || !strings.Contains(v.Message, "AGY") {
				t.Fatalf("combined health state wrong: %+v", v)
			}
		})
	}
}
