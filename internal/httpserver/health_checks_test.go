package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cliproxy-portal/internal/config"
	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/service"
	"cliproxy-portal/internal/store"
)

func TestReadDatabaseSpace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	for name, size := range map[string]int{"": 2048, "-wal": 512, "-shm": 32} {
		if err := os.WriteFile(path+name, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	space, err := readDatabaseSpace(path)
	if err != nil {
		t.Fatal(err)
	}
	if space.main != 2048 || space.wal != 512 || space.shm != 32 || space.total() != 2592 {
		t.Fatalf("database space = %+v, total %d", space, space.total())
	}
	if err := os.Remove(path + "-wal"); err != nil {
		t.Fatal(err)
	}
	space, err = readDatabaseSpace(path)
	if err != nil || space.wal != 0 || space.total() != 2080 {
		t.Fatalf("database without WAL = %+v, error %v", space, err)
	}
	if _, err := readDatabaseSpace(path + ".missing"); err == nil {
		t.Fatal("missing main database should fail")
	}
	if got := fileSizeLabel(1536); got != "1.5 KB" {
		t.Fatalf("size label = %q", got)
	}
}

func TestHostLogMetricsViews(t *testing.T) {
	path := filepath.Join(t.TempDir(), "host-log-metrics.json")
	write := func(generatedAt time.Time) {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"generated_at":           generatedAt,
			"cpa_main_log_bytes":     1048576,
			"cpa_response_log_bytes": 2097152,
			"container_log_bytes":    map[string]any{"portal": 1024, "cpamp": 2048, "cpa": 1024},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(time.Now().UTC())
	s := &Server{Cfg: config.Config{HostLogMetricsPath: path, TimeZone: time.UTC}}
	views := s.hostLogHealthViews()
	if len(views) != 3 || views[0].Metric != "1.0 MB" || views[1].Metric != "2.0 MB" || views[2].Metric != "4.0 KB" || !strings.Contains(views[2].Message, "CPAMP 2.0 KB") {
		t.Fatalf("host log views = %#v", views)
	}
	write(time.Now().Add(-10 * time.Minute))
	views = s.hostLogHealthViews()
	if views[0].Metric != "—" || views[0].Status != "warning" {
		t.Fatalf("stale host log view = %#v", views[0])
	}
}

func TestDatabaseHealthViewShowsDiskUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	st, err := store.Open(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{Cfg: config.Config{DatabasePath: path, TimeZone: time.UTC}, Store: st}
	v := s.databaseHealthView(t.Context())
	if v.Status != "healthy" || v.Metric == "—" || !strings.Contains(v.Message, "含主文件、WAL 和 SHM") {
		t.Fatalf("database health = %+v", v)
	}
}

func TestCPAMPDatabaseSize(t *testing.T) {
	got, err := cpampDatabaseSize(json.RawMessage(`{"databaseBytes":638275584,"walBytes":61865952,"shmBytes":32768,"totalBytes":700174304}`))
	if err != nil || got != 700174304 {
		t.Fatalf("CPAMP total = %d, %v", got, err)
	}
	got, err = cpampDatabaseSize(json.RawMessage(`{"databaseBytes":1024,"walBytes":512}`))
	if err != nil || got != 1536 {
		t.Fatalf("CPAMP fallback total = %d, %v", got, err)
	}
	if _, err = cpampDatabaseSize(json.RawMessage(`{"checkpoint":{}}`)); err == nil {
		t.Fatal("missing CPAMP size should fail")
	}
}

func TestCPAMPDatabaseHealthView(t *testing.T) {
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("CPAMP path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"service":"manager-server","database":{"totalBytes":1048576}}`))
	}))
	defer manager.Close()
	client, err := cpamp.New(manager.URL, "test-admin-key")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Cfg: config.Config{TimeZone: time.UTC}, Keys: service.NewKeys(nil, client)}
	v := s.cpampDatabaseHealthView(t.Context())
	if v.Status != "healthy" || v.Metric != "1.0 MB" {
		t.Fatalf("CPAMP database health = %+v", v)
	}
}

func TestCaptureHealthViewShowsDirectoryUsage(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"message.request.enc": 1024, filepath.Join("shared", "blob.enc"): 512} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Cfg: config.Config{TimeZone: time.UTC, GatewayListenAddr: ":18318", GatewayCaptureDir: dir}}
	v := s.captureHealthView()
	if v.Status != "healthy" || v.Metric != "1.5 KB" || !strings.Contains(v.Message, "存储可写") {
		t.Fatalf("capture health = %+v", v)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("capture probe left a temporary file: %v, %v", entries, err)
	}
}

func TestGatewayHealthView(t *testing.T) {
	var gatewayStatus atomic.Int32
	gatewayStatus.Store(http.StatusUnauthorized)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("gateway path = %q", r.URL.Path)
		}
		w.WriteHeader(int(gatewayStatus.Load()))
	}))
	defer gateway.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	s := &Server{Cfg: config.Config{
		TimeZone:          time.UTC,
		GatewayListenAddr: strings.TrimPrefix(gateway.URL, "http://"),
		CPAUpstreamURL:    upstream.URL,
		GatewayCaptureDir: dir,
	}}
	v := s.gatewayHealthView(t.Context())
	if v.Status != "healthy" || v.Metric != "服务可用" || !strings.Contains(v.Message, "网关鉴权正常") {
		t.Fatalf("gateway health = %+v", v)
	}
	upstreamView := s.cpaUpstreamHealthView(t.Context())
	if upstreamView.Status != "healthy" || upstreamView.Metric != "服务可用" {
		t.Fatalf("CPA upstream health = %+v", upstreamView)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("health probe left capture files: %v, %v", entries, err)
	}
	gatewayStatus.Store(http.StatusOK)
	v = s.gatewayHealthView(t.Context())
	if v.Status != "error" || !strings.Contains(v.Message, "网关鉴权异常") {
		t.Fatalf("gateway without auth = %+v", v)
	}
	gatewayStatus.Store(http.StatusUnauthorized)
	upstream.Close()
	upstreamView = s.cpaUpstreamHealthView(t.Context())
	if upstreamView.Status != "error" || !strings.Contains(upstreamView.Message, "CPA 模型接口不可达") {
		t.Fatalf("CPA upstream down = %+v", upstreamView)
	}
	s.Cfg.GatewayListenAddr = ""
	v = s.gatewayHealthView(t.Context())
	if v.StatusLabel != "未启用" {
		t.Fatalf("disabled gateway health = %+v", v)
	}
}

func TestGatewayProbeURL(t *testing.T) {
	if got, err := gatewayProbeURL(":18318"); err != nil || got != "http://127.0.0.1:18318/v1/models" {
		t.Fatalf("gateway probe URL = %q, %v", got, err)
	}
	if got, err := cpaProbeURL("http://cpa:8317/prefix"); err != nil || got != "http://cpa:8317/prefix/v1/models" {
		t.Fatalf("CPA probe URL = %q, %v", got, err)
	}
	if _, err := cpaProbeURL("http://user:secret@cpa:8317"); err == nil {
		t.Fatal("CPA probe URL must reject embedded credentials")
	}
}
