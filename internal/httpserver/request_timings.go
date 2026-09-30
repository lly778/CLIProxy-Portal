package httpserver

import (
	"fmt"
	"strings"

	"cliproxy-portal/internal/cpamp"
	"cliproxy-portal/internal/store"
)

func requestTimingDisplay(row store.GatewayRequestTiming, event cpamp.EventRow) (string, string) {
	total := fmt.Sprintf("%d ms", row.TotalMS)
	lines := []string{"服务器总耗时：" + total}
	if !row.Completed {
		lines = append(lines, "连接中断或回复未完整写出；总耗时为中断前已耗时间")
	}
	model := "—"
	if event.LatencyMS != nil {
		model = fmt.Sprintf("%d ms", *event.LatencyMS)
	}
	lines = append(lines, "", "各阶段耗时（完整记录时，三段相加为总耗时）")
	stage := func(label string, from, to *int64) {
		value := "—"
		if from != nil && to != nil && *to >= *from {
			value = fmt.Sprintf("%d ms", *to-*from)
		}
		lines = append(lines, label+"："+value)
	}
	zero := int64(0)
	stage("接入及请求上传", &zero, row.RequestReadMS)
	stage("请求收齐到开始写出回复", row.RequestReadMS, row.ResponseStartedMS)
	stage("持续生成及发送回复", row.ResponseStartedMS, &row.TotalMS)
	lines = append(lines, "", "模型耗时指标（与上述阶段重合，不参与累加）", "模型调用耗时："+model)
	if event.TTFTMS != nil {
		lines = append(lines, fmt.Sprintf("其中：首 token 等待 %d ms", *event.TTFTMS))
	}
	lines = append(lines, "")
	switch row.StartSource {
	case "connection_accepted":
		lines = append(lines, "计时起点：服务器接入新连接")
	case "request_received":
		lines = append(lines, "计时起点：复用连接收到本次请求")
	case "headers_received":
		lines = append(lines, "计时起点：已缓冲的请求头解析完成；此前接收时间不可分离")
	default:
		lines = append(lines, "计时起点：开始处理请求；此前连接及请求头接收未记录")
	}
	lines = append(lines, "计时终点：服务器完成回复写出或连接中断")
	return total, strings.Join(lines, "\n")
}
