package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cliproxy-portal/internal/webui"
)

type databaseSpace struct {
	main int64
	wal  int64
	shm  int64
}

func (s databaseSpace) total() int64 { return s.main + s.wal + s.shm }

func readDatabaseSpace(path string) (databaseSpace, error) {
	if strings.TrimSpace(path) == "" {
		return databaseSpace{}, errors.New("database path is empty")
	}
	var space databaseSpace
	for index, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(path + suffix)
		if os.IsNotExist(err) && index != 0 {
			continue
		}
		if err != nil {
			return databaseSpace{}, err
		}
		if !info.Mode().IsRegular() {
			return databaseSpace{}, errors.New("database component is not a regular file")
		}
		switch index {
		case 0:
			space.main = info.Size()
		case 1:
			space.wal = info.Size()
		case 2:
			space.shm = info.Size()
		}
	}
	return space, nil
}

func fileSizeLabel(size int64) string {
	switch {
	case size < 1<<10:
		return fmt.Sprintf("%d B", size)
	case size < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	case size < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(size)/(1<<30))
	}
}

func (s *Server) databaseHealthView(ctx context.Context) webui.HealthCheckView {
	start := time.Now()
	err := s.Store.Ping(ctx)
	v := healthView("门户 SQLite", err, time.Since(start), s)
	v.Metric = "—"
	v.Latency = ""
	if err != nil {
		return v
	}
	space, err := readDatabaseSpace(s.Cfg.DatabasePath)
	if err != nil {
		v.Status, v.StatusLabel = "warning", "需关注"
		v.Message = "数据库连接正常，占用空间暂不可读取"
		return v
	}
	v.Metric = fileSizeLabel(space.total())
	v.Message = "数据库正常 · 含主文件、WAL 和 SHM"
	return v
}

func cpampDatabaseSize(raw json.RawMessage) (int64, error) {
	var size struct {
		DatabaseBytes *int64 `json:"databaseBytes"`
		WALBytes      *int64 `json:"walBytes"`
		SHMBytes      *int64 `json:"shmBytes"`
		TotalBytes    *int64 `json:"totalBytes"`
	}
	if err := json.Unmarshal(raw, &size); err != nil {
		return 0, err
	}
	if size.TotalBytes != nil {
		if *size.TotalBytes < 0 {
			return 0, errors.New("negative CPAMP database size")
		}
		return *size.TotalBytes, nil
	}
	if size.DatabaseBytes == nil || *size.DatabaseBytes < 0 {
		return 0, errors.New("CPAMP database size is unavailable")
	}
	total := *size.DatabaseBytes
	for _, part := range []*int64{size.WALBytes, size.SHMBytes} {
		if part != nil {
			if *part < 0 {
				return 0, errors.New("negative CPAMP database component size")
			}
			total += *part
		}
	}
	return total, nil
}

func (s *Server) cpampDatabaseHealthView(ctx context.Context) webui.HealthCheckView {
	start := time.Now()
	status, statusErr := s.Keys.CPAMP.Status(ctx)
	v := healthView("CPAMP SQLite", statusErr, time.Since(start), s)
	v.Metric = "—"
	if statusErr != nil {
		if _, healthErr := s.Keys.CPAMP.Health(ctx); healthErr == nil {
			v.Status, v.StatusLabel = "warning", "需关注"
			v.Message = "管理服务可达，SQLite 占用暂不可读取"
			v.Latency = time.Since(start).Round(time.Millisecond).String()
		}
		return v
	}
	bytes, err := cpampDatabaseSize(status.Database)
	if err != nil {
		v.Status, v.StatusLabel = "warning", "需关注"
		v.Message = "管理服务正常，SQLite 占用暂不可读取"
		return v
	}
	v.Metric = fileSizeLabel(bytes)
	v.Message = "管理服务正常 · 含主文件、WAL 和 SHM"
	return v
}

func readCaptureSpace(dir string) (int64, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, errors.New("capture directory is empty")
	}
	var total int64
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (s *Server) captureHealthView() webui.HealthCheckView {
	v := webui.HealthCheckView{Component: "交互记录存储", Metric: "—", CheckedAt: s.formatTime(time.Now())}
	if s.Cfg.GatewayListenAddr == "" {
		v.Status, v.StatusLabel, v.Message = "warning", "未启用", "网关未配置"
		return v
	}
	bytes, err := readCaptureSpace(s.Cfg.GatewayCaptureDir)
	writable := probeCaptureStorage(s.Cfg.GatewayCaptureDir)
	if err == nil {
		v.Metric = fileSizeLabel(bytes)
	}
	switch {
	case err == nil && writable:
		v.Status, v.StatusLabel, v.Message = "healthy", "正常", "加密交互文件 · 存储可写"
	case err == nil:
		v.Status, v.StatusLabel, v.Message = "error", "异常", "加密交互文件 · 存储不可写"
	default:
		v.Status, v.StatusLabel, v.Message = "error", "异常", "交互记录目录暂不可读取"
	}
	return v
}

func gatewayProbeURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return "", errors.New("invalid gateway listen address")
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/v1/models", nil
}

func cpaProbeURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid CPA upstream URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/models"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func probeModelList(ctx context.Context, endpoint string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	_ = response.Body.Close()
	return response.StatusCode, nil
}

func probeCaptureStorage(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	file, err := os.CreateTemp(dir, ".gateway-health-*")
	if err != nil {
		return false
	}
	name := file.Name()
	_, writeErr := file.Write([]byte{0})
	closeErr := file.Close()
	removeErr := os.Remove(name)
	return writeErr == nil && closeErr == nil && removeErr == nil
}

func (s *Server) gatewayHealthView(ctx context.Context) webui.HealthCheckView {
	v := webui.HealthCheckView{Component: "模型网关", Metric: "—", CheckedAt: s.formatTime(time.Now())}
	if s.Cfg.GatewayListenAddr == "" {
		v.Status, v.StatusLabel, v.Metric, v.Message = "warning", "未启用", "未启用", "网关未配置"
		return v
	}
	start := time.Now()
	gatewayURL, gatewayErr := gatewayProbeURL(s.Cfg.GatewayListenAddr)
	var gatewayStatus int
	if gatewayErr == nil {
		gatewayStatus, gatewayErr = probeModelList(ctx, gatewayURL)
	}
	gatewayOK := gatewayErr == nil && (gatewayStatus == http.StatusUnauthorized || gatewayStatus == http.StatusForbidden)
	v.Message = "网关鉴权正常"
	if !gatewayOK {
		v.Message = "网关请求失败"
		if gatewayErr == nil && gatewayStatus >= 200 && gatewayStatus < 400 {
			v.Message = "网关鉴权异常"
		}
	}
	v.Latency = time.Since(start).Round(time.Millisecond).String()
	if gatewayOK {
		v.Status, v.StatusLabel, v.Metric = "healthy", "正常", "服务可用"
	} else {
		v.Status, v.StatusLabel, v.Metric = "error", "异常", "需检查"
	}
	return v
}

func (s *Server) cpaUpstreamHealthView(ctx context.Context) webui.HealthCheckView {
	v := webui.HealthCheckView{Component: "CPA 上游", Metric: "—", CheckedAt: s.formatTime(time.Now())}
	if s.Cfg.CPAUpstreamURL == "" {
		v.Status, v.StatusLabel, v.Metric, v.Message = "warning", "未配置", "未配置", "未设置 CPA 上游地址"
		return v
	}
	start := time.Now()
	endpoint, err := cpaProbeURL(s.Cfg.CPAUpstreamURL)
	var status int
	if err == nil {
		status, err = probeModelList(ctx, endpoint)
	}
	v.Latency = time.Since(start).Round(time.Millisecond).String()
	if err == nil && status >= 200 && status < 500 && status != http.StatusNotFound {
		v.Status, v.StatusLabel, v.Metric, v.Message = "healthy", "正常", "服务可用", "CPA 模型接口可达"
	} else {
		v.Status, v.StatusLabel, v.Metric, v.Message = "error", "异常", "需检查", "CPA 模型接口不可达"
	}
	return v
}
