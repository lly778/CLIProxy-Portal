package httpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"cliproxy-portal/internal/webui"
)

const hostLogMetricsMaxAge = 5 * time.Minute

type hostLogMetrics struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	CPAMainLogBytes  *int64            `json:"cpa_main_log_bytes"`
	CPAResponseBytes *int64            `json:"cpa_response_log_bytes"`
	ContainerBytes   map[string]*int64 `json:"container_log_bytes"`
}

func readHostLogMetrics(path string, now time.Time) (hostLogMetrics, error) {
	if strings.TrimSpace(path) == "" {
		return hostLogMetrics{}, errors.New("host log metrics path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return hostLogMetrics{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return hostLogMetrics{}, errors.New("invalid host log metrics file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return hostLogMetrics{}, err
	}
	var metrics hostLogMetrics
	if err := json.Unmarshal(data, &metrics); err != nil {
		return hostLogMetrics{}, err
	}
	if metrics.GeneratedAt.IsZero() || now.Sub(metrics.GeneratedAt) > hostLogMetricsMaxAge || metrics.GeneratedAt.Sub(now) > time.Minute {
		return hostLogMetrics{}, errors.New("host log metrics are stale")
	}
	for _, value := range []*int64{metrics.CPAMainLogBytes, metrics.CPAResponseBytes, metrics.ContainerBytes["portal"], metrics.ContainerBytes["cpamp"], metrics.ContainerBytes["cpa"]} {
		if value != nil && *value < 0 {
			return hostLogMetrics{}, errors.New("negative host log size")
		}
	}
	return metrics, nil
}

func (s *Server) hostLogHealthViews() []webui.HealthCheckView {
	now := time.Now()
	metrics, err := readHostLogMetrics(s.Cfg.HostLogMetricsPath, now)
	newView := func(component string) webui.HealthCheckView {
		return webui.HealthCheckView{Component: component, Status: "warning", StatusLabel: "待采集", Metric: "—", Message: "宿主机占用数据暂不可用", CheckedAt: s.formatTime(now)}
	}
	main, responses, docker := newView("CPA 主日志"), newView("CPA 请求/响应日志"), newView("容器运行日志")
	if err != nil {
		return []webui.HealthCheckView{main, responses, docker}
	}
	checkedAt := s.formatTime(metrics.GeneratedAt)
	if metrics.CPAMainLogBytes != nil {
		main.Status, main.StatusLabel, main.Metric = "healthy", "正常", fileSizeLabel(*metrics.CPAMainLogBytes)
		main.Message, main.CheckedAt = "CPA main.log 文件占用", checkedAt
	}
	if metrics.CPAResponseBytes != nil {
		responses.Status, responses.StatusLabel, responses.Metric = "healthy", "正常", fileSizeLabel(*metrics.CPAResponseBytes)
		responses.Message, responses.CheckedAt = "CPA v1-responses 日志合计", checkedAt
	}
	var total int64
	var parts []string
	allAvailable := true
	available := 0
	for _, container := range []struct{ key, label string }{{"portal", "门户"}, {"cpamp", "CPAMP"}, {"cpa", "CPA"}} {
		value := metrics.ContainerBytes[container.key]
		if value == nil {
			allAvailable = false
			parts = append(parts, container.label+" —")
			continue
		}
		total += *value
		available++
		parts = append(parts, fmt.Sprintf("%s %s", container.label, fileSizeLabel(*value)))
	}
	docker.Message, docker.CheckedAt = strings.Join(parts, " · "), checkedAt
	if allAvailable {
		docker.Status, docker.StatusLabel, docker.Metric = "healthy", "正常", fileSizeLabel(total)
	} else {
		docker.StatusLabel = "部分不可用"
		if available > 0 {
			docker.Metric = fileSizeLabel(total) + "（已获取部分）"
		}
	}
	return []webui.HealthCheckView{main, responses, docker}
}
