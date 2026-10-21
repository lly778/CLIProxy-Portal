package httpserver

import "strings"

// Local protection records include historical Chinese messages. Translate only
// their presentation; keep stored metadata and original upstream errors intact.
func requestLogMessage(message string) string {
	message = strings.TrimSpace(message)
	switch message {
	case "全局并发已满":
		return "Global concurrency limit reached"
	case "用户并发已满":
		return "User concurrency limit reached"
	case "交互记录待处理过多":
		return "Interaction capture backlog limit reached"
	case "整机内存达到总上限":
		return "Server memory limit reached"
	case "CPU 持续繁忙":
		return "Server CPU limit exceeded"
	case "CPA套件内存达到上限", "CPA 套件内存达到上限":
		return "CPA suite memory limit reached"
	case "CPA套件 CPU 持续繁忙", "CPA 套件 CPU 持续繁忙":
		return "CPA suite CPU limit exceeded"
	case "资源监测未连接":
		return "Resource monitor unavailable"
	case "请求超过大小上限":
		return "Request size limit exceeded"
	case "请求过大或内容无效":
		return "Request too large or invalid"
	}
	return message
}
